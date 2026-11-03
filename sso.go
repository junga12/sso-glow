package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Profile 은 ~/.aws/config 에 정의된 SSO 프로필 하나다.
type Profile struct {
	Name    string // [profile NAME] 의 NAME
	Session string // sso_session 값
	Account string // sso_account_id (표시용)
	Role    string // sso_role_name (표시용)
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

// loadProfiles 는 config 파일에서 sso_session 이 설정된 프로필만 파일 순서대로 돌려준다.
// [default] 섹션도 sso_session 이 있으면 "default" 이름으로 포함한다.
// 두 번째 반환값은 이름이 유효하지 않아 건너뛴 프로필들이다.
func loadProfiles(path string) ([]Profile, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	return parseProfiles(f)
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

func parseProfiles(r io.Reader) ([]Profile, []string, error) {
	var (
		profiles []Profile
		cur      *Profile // 현재 읽고 있는 프로필 섹션. 프로필이 아니면 nil
		order    []string
		byName   = map[string]*Profile{}
	)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			cur = nil
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
			default:
				continue // sso-session, services 등은 프로필이 아니다
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
		if cur == nil {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "sso_session":
			cur.Session = val
		case "sso_account_id":
			cur.Account = val
		case "sso_role_name":
			cur.Role = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	var skipped []string
	for _, name := range order {
		p := byName[name]
		if p.Session == "" {
			continue
		}
		if !validName(p.Name) || !validName(p.Session) {
			skipped = append(skipped, p.Name)
			continue
		}
		profiles = append(profiles, *p)
	}
	return profiles, skipped, nil
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

// runner 는 aws CLI 호출을 추상화한다. 테스트에서는 가짜 구현으로 바꾼다.
type runner interface {
	// Check 는 프로필의 세션이 유효하면 nil, 아니면 사용자에게 보여줄 에러를 돌려준다.
	Check(ctx context.Context, profile string) error
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

// awsRunner 는 실제 aws CLI 를 실행한다.
type awsRunner struct {
	bin string
}

// newAWSRunner 는 aws 실행 파일을 찾는다. GUI 앱으로 실행되면 PATH 가 최소한으로 잡혀
// 있어서 Homebrew 경로 등을 직접 찾아본다.
func newAWSRunner() (*awsRunner, error) {
	if p, err := exec.LookPath("aws"); err == nil {
		return &awsRunner{bin: p}, nil
	}
	for _, p := range []string{
		"/opt/homebrew/bin/aws",
		"/usr/local/bin/aws",
		"/usr/bin/aws",
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return &awsRunner{bin: p}, nil
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

func (r *awsRunner) Check(ctx context.Context, profile string) error {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bin, "sts", "get-caller-identity", "--profile", profile, "--output", "json")
	cmd.Env = r.env()
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return cliError(ctx, err, stderr.String())
	}
	return nil
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
		if r := []rune(line); len(r) > 160 {
			line = string(r[:160]) + "…"
		}
		return errors.New(line)
	}
	return fmt.Errorf("aws 실행 실패: %v", err)
}
