package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ssooidc"
	ssooidctypes "github.com/aws/aws-sdk-go-v2/service/ssooidc/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// Profile 은 ~/.aws/config 에 정의된 SSO 프로필 하나다.
type Profile struct {
	Name    string // [profile NAME] 의 NAME
	Session string // sso_session 값
	Account string // sso_account_id (표시용)
	Role    string // sso_role_name (표시용)
	Region  string // region. 비어 있으면 검사 시 sso-session 의 sso_region 을 쓴다
}

// Session 은 [sso-session NAME] 섹션이다.
type Session struct {
	Name   string
	Region string // sso_region. 토큰 갱신 요청을 보낼 Identity Center 리전
}

// Config 는 config 파일에서 앱이 쓰는 부분만 뽑은 것이다.
type Config struct {
	Profiles []Profile          // sso_session 이 설정된 프로필, 파일 순서
	Sessions map[string]Session // sso-session 이름 → 설정
	Skipped  []string           // 이름이 유효하지 않아 건너뛴 프로필
}

// defaultConfigPath 는 AWS_CONFIG_FILE 또는 ~/.aws/config 를 돌려준다.
func defaultConfigPath() string {
	if p := os.Getenv("AWS_CONFIG_FILE"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".aws", "config")
}

// loadConfig 는 config 파일을 읽어 SSO 프로필과 sso-session 정의를 돌려준다.
func loadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseConfig(f)
}

// validName 은 aws CLI 인자로 그대로 넘겨도 안전한 이름인지 검사한다.
// '-' 로 시작하면 플래그로 해석되고, 공백·제어 문자는 설정 오류이므로 거른다.
func validName(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// parseConfig 는 [profile X]·[default] 섹션에서 sso_session 이 설정된 프로필을,
// [sso-session X] 섹션에서 sso_region 을 읽는다.
func parseConfig(r io.Reader) (*Config, error) {
	var (
		cur     *Profile // 현재 읽고 있는 프로필 섹션. 프로필이 아니면 nil
		curSess *Session // 현재 읽고 있는 sso-session 섹션
		order   []string
		byName  = map[string]*Profile{}
		sess    = map[string]Session{}
	)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			cur, curSess = nil, nil
			end := strings.IndexByte(line, ']')
			if end < 0 {
				continue
			}
			sec := strings.TrimSpace(line[1:end])
			var name string
			switch {
			case sec == "default":
				name = "default"
			case strings.HasPrefix(sec, "profile "):
				name = strings.TrimSpace(strings.TrimPrefix(sec, "profile "))
			case strings.HasPrefix(sec, "sso-session "):
				n := strings.TrimSpace(strings.TrimPrefix(sec, "sso-session "))
				if n == "" {
					continue
				}
				s := sess[n]
				s.Name = n
				curSess = &s
				continue
			default:
				continue // services 등은 프로필이 아니다
			}
			if name == "" {
				continue
			}
			if p, ok := byName[name]; ok {
				cur = p
			} else {
				p := &Profile{Name: name}
				byName[name] = p
				order = append(order, name)
				cur = p
			}
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch {
		case curSess != nil:
			if key == "sso_region" {
				curSess.Region = val
			}
			sess[curSess.Name] = *curSess
		case cur != nil:
			switch key {
			case "sso_session":
				cur.Session = val
			case "sso_account_id":
				cur.Account = val
			case "sso_role_name":
				cur.Role = val
			case "region":
				cur.Region = val
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	cfg := &Config{Sessions: sess}
	for _, name := range order {
		p := byName[name]
		if p.Session == "" {
			continue
		}
		if !validName(p.Name) || !validName(p.Session) {
			cfg.Skipped = append(cfg.Skipped, p.Name)
			continue
		}
		cfg.Profiles = append(cfg.Profiles, *p)
	}
	return cfg, nil
}

// sessionsOf 는 프로필들이 참조하는 sso_session 이름을 중복 없이 처음 등장한 순서로 돌려준다.
func sessionsOf(profiles []Profile) []string {
	var out []string
	for _, p := range profiles {
		if p.Session != "" && !slices.Contains(out, p.Session) {
			out = append(out, p.Session)
		}
	}
	return out
}

// runner 는 AWS 호출을 추상화한다. 테스트에서는 가짜 구현으로 바꾼다.
type runner interface {
	// CheckSession 은 sso-session 의 로그인 세션이 살아 있는지 검사한다. 캐시된 액세스
	// 토큰이 만료됐으면 리프레시 토큰으로 갱신을 시도하고, 거절되면 에러를 돌려준다.
	// 같은 세션을 동시에 갱신하면 리프레시 토큰이 교체되어 뒤의 요청이 거절되므로
	// 호출자는 세션마다 한 번씩 순차로 불러야 한다.
	CheckSession(ctx context.Context, s Session) error
	// Check 는 프로필로 실제 API 를 호출해 역할 자격증명을 받을 수 있는지 검사한다.
	// 유효하면 nil, 아니면 사용자에게 보여줄 에러를 돌려준다.
	Check(ctx context.Context, p Profile) error
	// Login 은 sso-session 단위로 로그인한다. aws CLI 가 직접 브라우저를 열고,
	// 그 과정에서 출력하는 인증 URL 과 코드를 progress 로 전달한다.
	Login(ctx context.Context, session string, progress func(LoginProgress)) error
}

// LoginProgress 는 aws sso login 이 출력한 인증 정보다. 브라우저가 자동으로
// 열리지 않을 때 사용자가 직접 열 수 있도록 화면에 보여준다.
type LoginProgress struct {
	URL  string // 인증 페이지. PKCE 방식이면 쿼리 문자열이 길다
	Code string // 디바이스 코드 방식일 때만 있다 (예: ABCD-EFGH)
}

var (
	loginURLRe  = regexp.MustCompile(`https://\S+`)
	loginCodeRe = regexp.MustCompile(`^[A-Z0-9]{4}-[A-Z0-9]{4}$`)
)

// parseLoginLine 은 aws sso login 의 출력 한 줄에서 URL 또는 코드를 찾아 prog 에 채운다.
// 새 정보가 있으면 true 를 돌려준다.
func parseLoginLine(line string, prog *LoginProgress) bool {
	line = strings.TrimSpace(line)
	if u := loginURLRe.FindString(line); u != "" {
		prog.URL = u
		return true
	}
	if loginCodeRe.MatchString(line) {
		prog.Code = line
		return true
	}
	return false
}

const (
	checkTimeout = 30 * time.Second
	loginTimeout = 5 * time.Minute
)

// awsRunner 는 검사는 AWS SDK 로, 로그인은 aws CLI 로 수행한다.
//
// 검사에 CLI 를 쓰지 않는 이유: CLI 는 역할 자격증명을 ~/.aws/cli/cache 에 디스크 캐시로
// 두기 때문에, SSO 로그인 세션이 이미 끝났어도 캐시가 남아 있는 동안은 성공한다.
// SDK 는 매번 ~/.aws/sso/cache 의 토큰 경로를 타므로 다른 SDK 기반 도구와 같은 판정을 낸다.
type awsRunner struct {
	bin        string // aws CLI 경로 (로그인용)
	configPath string
}

// newAWSRunner 는 aws 실행 파일을 찾는다. GUI 앱으로 실행되면 PATH 가 최소한으로 잡혀
// 있어서 Homebrew 경로 등을 직접 찾아본다.
func newAWSRunner(configPath string) (*awsRunner, error) {
	if p, err := exec.LookPath("aws"); err == nil {
		return &awsRunner{bin: p, configPath: configPath}, nil
	}
	for _, p := range []string{
		"/opt/homebrew/bin/aws",
		"/usr/local/bin/aws",
		"/usr/bin/aws",
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return &awsRunner{bin: p, configPath: configPath}, nil
		}
	}
	return nil, errors.New("aws CLI 를 찾을 수 없습니다. PATH 또는 /opt/homebrew/bin 을 확인하세요")
}

// env 는 aws CLI 가 브라우저(open)와 보조 도구를 찾을 수 있도록 PATH 를 보강한다.
func (r *awsRunner) env() []string {
	path := os.Getenv("PATH")
	for _, d := range []string{filepath.Dir(r.bin), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"} {
		if !strings.Contains(":"+path+":", ":"+d+":") {
			path += ":" + d
		}
	}
	env := os.Environ()
	env = slices.DeleteFunc(env, func(s string) bool {
		return strings.HasPrefix(s, "PATH=") || strings.HasPrefix(s, "PYTHONUNBUFFERED=")
	})
	// aws CLI 는 Python 이라 stdout 이 파이프면 버퍼링한다. 로그인 URL 을 즉시 받으려면 끈다.
	return append(env, "PATH="+strings.TrimPrefix(path, ":"), "PYTHONUNBUFFERED=1")
}

func (r *awsRunner) CheckSession(ctx context.Context, s Session) error {
	if s.Region == "" {
		return fmt.Errorf("config 에 [sso-session %s] 섹션 또는 sso_region 이 없습니다", s.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	// aws CLI 와 같은 위치(~/.aws/sso/cache/<sha1(세션 이름)>.json)를 쓴다.
	path, err := ssocreds.StandardCachedTokenFilepath(s.Name)
	if err != nil {
		return sdkError(ctx, err)
	}
	client := ssooidc.New(ssooidc.Options{Region: s.Region})
	// 토큰 값은 쓰지 않고 버린다. 만료됐으면 SDK 가 리프레시 토큰으로 갱신해 같은 파일에
	// 같은 권한으로 다시 쓴다. 갱신이 거절되면 로그인 세션이 끝난 것이다.
	_, err = ssocreds.NewSSOTokenProvider(client, path).RetrieveBearerToken(ctx)
	return sdkError(ctx, err)
}

func (r *awsRunner) Check(ctx context.Context, p Profile) error {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	opts := []func(*config.LoadOptions) error{
		config.WithSharedConfigProfile(p.Name),
		config.WithSharedConfigFiles([]string{r.configPath}),
		config.WithSharedCredentialsFiles([]string{}), // ~/.aws/credentials 는 읽지 않는다
		config.WithEC2IMDSClientEnableState(imds.ClientDisabled),
	}
	if p.Region != "" {
		opts = append(opts, config.WithDefaultRegion(p.Region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return sdkError(ctx, err)
	}
	// 역할 자격증명은 프로세스 메모리에만 머물고 디스크에 캐시하지 않는다.
	_, err = sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	return sdkError(ctx, err)
}

// sdkError 는 SDK 에러를 사용자에게 보여줄 한 줄로 바꾼다. 토큰이나 요청 본문은 담지 않는다.
func sdkError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return errors.New("시간 초과")
	}
	var grant *ssooidctypes.InvalidGrantException
	var token *ssocreds.InvalidTokenError
	switch {
	case errors.As(err, &grant):
		// 리프레시 토큰이 거절됨: 포털에서 로그아웃했거나 관리자가 세션을 끊었다
		return errors.New("SSO 로그인 세션이 끝났습니다. 다시 로그인하세요")
	case errors.As(err, &token), strings.Contains(err.Error(), "cannot be refreshed"):
		// 액세스 토큰이 만료됐는데 리프레시 토큰이 없다 (sso_registration_scopes 미설정 등)
		return errors.New("SSO 토큰이 만료되었습니다. 다시 로그인하세요")
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("SSO 로그인 기록이 없습니다. 로그인하세요")
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		return errors.New(shorten(api.ErrorCode() + ": " + api.ErrorMessage()))
	}
	return errors.New(shorten(err.Error()))
}

func (r *awsRunner) Login(ctx context.Context, session string, progress func(LoginProgress)) error {
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bin, "sso", "login", "--sso-session", session)
	cmd.Env = r.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("aws 실행 실패: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("aws 실행 실패: %v", err)
	}
	// stdout 을 줄 단위로 읽어 URL 과 코드가 나오는 즉시 알린다. 내용은 저장하지 않는다.
	var prog LoginProgress
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if parseLoginLine(sc.Text(), &prog) && progress != nil {
			progress(prog)
		}
	}
	if err := cmd.Wait(); err != nil {
		return cliError(ctx, err, stderr.String())
	}
	return nil
}

// cliError 는 aws CLI 의 stderr 에서 사람이 읽을 한 줄을 뽑는다.
// 자격증명이나 토큰이 섞일 수 있으므로 첫 줄만, 길이를 제한해 돌려준다.
func cliError(ctx context.Context, err error, stderr string) error {
	if ctx.Err() == context.DeadlineExceeded {
		return errors.New("시간 초과")
	}
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "aws: [ERROR]: ")
		line = strings.TrimPrefix(line, "Error when retrieving token from sso: ")
		return errors.New(shorten(line))
	}
	return fmt.Errorf("aws 실행 실패: %v", err)
}

// shorten 은 화면에 보여줄 한 줄을 160자로 자른다.
func shorten(s string) string {
	if r := []rune(s); len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return s
}
