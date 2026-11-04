package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"
)

// fakeRunner 는 AWS 호출 대신 미리 정한 결과를 돌려준다.
type fakeRunner struct {
	expired  map[string]string // sso-session → 에러 메시지 (CheckSession 실패)
	invalid  map[string]string // 프로필 → 에러 메시지 (Check 실패)
	sessions []Session         // CheckSession 이 받은 세션 순서
	checked  []Profile         // Check 가 받은 프로필
	logins   []string          // Login 이 호출된 세션 순서
	loginErr error
	progress LoginProgress // Login 중 progress 로 알릴 값. URL 이 비면 알리지 않는다
}

func (f *fakeRunner) CheckSession(_ context.Context, s Session) error {
	f.sessions = append(f.sessions, s)
	if msg, ok := f.expired[s.Name]; ok {
		return errors.New(msg)
	}
	return nil
}

func (f *fakeRunner) Check(_ context.Context, p Profile) error {
	f.checked = append(f.checked, p)
	if msg, ok := f.invalid[p.Name]; ok {
		return errors.New(msg)
	}
	return nil
}

func (f *fakeRunner) Login(_ context.Context, session string, progress func(LoginProgress)) error {
	f.logins = append(f.logins, session)
	if f.progress.URL != "" && progress != nil {
		progress(f.progress)
	}
	if f.loginErr != nil {
		return f.loginErr
	}
	// 로그인하면 모든 세션과 프로필이 유효해진다
	f.expired = nil
	f.invalid = nil
	return nil
}

func writeConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte(sampleConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// newTestApp 은 백그라운드 작업을 동기로 실행하는 앱을 만든다.
func newTestApp(t *testing.T, run runner) *app {
	t.Helper()
	a := newApp(writeConfig(t), run)
	a.spawn = func(fn func()) { fn() }
	return a
}

func TestViewCheckAndLogin(t *testing.T) {
	run := &fakeRunner{invalid: map[string]string{
		"dev":       "Token has expired and refresh failed",
		"other-sso": "Token has expired and refresh failed",
	}}
	a := newTestApp(t, run)
	tt := ui.NewTester(a.view, 560, 440)

	if !tt.HasText("미확인") {
		t.Errorf("검사 전에는 미확인이어야 한다: %q", tt.Texts())
	}

	if err := tt.Click("다시 확인"); err != nil {
		t.Fatal(err)
	}
	if a.busy {
		t.Fatal("동기 실행 후에는 busy 가 풀려야 한다")
	}
	if !tt.HasText("만료") || !tt.HasText("유효") {
		t.Errorf("만료/유효 배지가 보여야 한다: %q", tt.Texts())
	}
	if !tt.HasText("3개 프로필 중 2개의 세션이 유효하지 않습니다.") {
		t.Errorf("요약 줄: %q", tt.Texts())
	}
	// dev 와 other-sso 가 만료 → corp, partner 두 세션
	if !tt.HasText("SSO 로그인 (2개 세션)") {
		t.Errorf("로그인 버튼 라벨: %q", tt.Texts())
	}

	if err := tt.Click("SSO 로그인 (2개 세션)"); err != nil {
		t.Fatal(err)
	}
	if len(run.logins) != 2 || run.logins[0] != "corp" || run.logins[1] != "partner" {
		t.Errorf("세션 단위로 한 번씩 로그인해야 한다: %v", run.logins)
	}
	if tt.HasText("만료") || !tt.HasText("모든 AWS SSO 세션이 유효합니다. (3개 프로필)") {
		t.Errorf("로그인 후 재검사 결과: %q", tt.Texts())
	}
	if !tt.HasText("SSO 로그인") {
		t.Errorf("세션이 모두 유효하면 로그인 버튼은 남되 비활성이어야 한다: %q", tt.Texts())
	}
}

func TestSessionExpiredMarksAllProfiles(t *testing.T) {
	// 리프레시 토큰이 거절되면 그 세션을 쓰는 프로필은 개별 검사 없이 모두 만료다
	run := &fakeRunner{expired: map[string]string{
		"corp": "SSO 로그인 세션이 끝났습니다. 다시 로그인하세요",
	}}
	a := newTestApp(t, run)
	tt := ui.NewTester(a.view, 560, 440)
	if err := tt.Click("다시 확인"); err != nil {
		t.Fatal(err)
	}
	if len(run.sessions) != 2 || run.sessions[0].Name != "corp" || run.sessions[1].Name != "partner" {
		t.Errorf("세션마다 한 번씩 순서대로 검사해야 한다: %+v", run.sessions)
	}
	if run.sessions[0].Region != "ap-northeast-1" || run.sessions[1].Region != "" {
		t.Errorf("sso_region 은 config 에서, 정의가 없으면 빈 값: %+v", run.sessions)
	}
	// corp 를 쓰는 dev·default 는 Check 없이 만료, partner 의 other-sso 만 Check 한다
	if len(run.checked) != 1 || run.checked[0].Name != "other-sso" {
		t.Errorf("세션이 끝난 프로필은 개별 검사하지 않는다: %+v", run.checked)
	}
	if !tt.HasText("3개 프로필 중 2개의 세션이 유효하지 않습니다.") {
		t.Errorf("요약 줄: %q", tt.Texts())
	}
	if !tt.HasText("SSO 로그인 세션이 끝났습니다. 다시 로그인하세요") {
		t.Errorf("세션 에러가 프로필 줄에 보여야 한다: %q", tt.Texts())
	}
	if !tt.HasText("SSO 로그인") || tt.HasText("SSO 로그인 (2개 세션)") {
		t.Errorf("corp 하나만 로그인 대상이어야 한다: %q", tt.Texts())
	}
	if err := tt.Click("SSO 로그인"); err != nil {
		t.Fatal(err)
	}
	if len(run.logins) != 1 || run.logins[0] != "corp" {
		t.Errorf("logins=%v", run.logins)
	}
	if !tt.HasText("모든 AWS SSO 세션이 유효합니다. (3개 프로필)") {
		t.Errorf("로그인 후 재검사: %q", tt.Texts())
	}
}

func TestProfileRegionFallsBackToSession(t *testing.T) {
	run := &fakeRunner{}
	a := newTestApp(t, run)
	a.checkAll()
	byName := map[string]Profile{}
	for _, p := range run.checked {
		byName[p.Name] = p
	}
	if byName["dev"].Region != "ap-northeast-1" {
		t.Errorf("프로필의 region 유지: %+v", byName["dev"])
	}
	if byName["default"].Region != "ap-northeast-1" {
		t.Errorf("region 이 없으면 sso_region 으로 대체: %+v", byName["default"])
	}
	if byName["other-sso"].Region != "" {
		t.Errorf("세션 정의도 없으면 빈 값: %+v", byName["other-sso"])
	}
}

func TestViewLoginFailure(t *testing.T) {
	run := &fakeRunner{
		invalid:  map[string]string{"dev": "expired"},
		loginErr: errors.New("브라우저 인증이 취소됨"),
	}
	a := newTestApp(t, run)
	tt := ui.NewTester(a.view, 560, 440)
	if err := tt.Click("다시 확인"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("SSO 로그인"); err != nil {
		t.Fatal(err)
	}
	if a.busy || !tt.HasText("SSO 세션(corp) 로그인 실패: 브라우저 인증이 취소됨") {
		t.Errorf("실패 메시지가 보이고 busy 가 풀려야 한다: busy=%v texts=%q", a.busy, tt.Texts())
	}
}

func TestStaleResultIgnored(t *testing.T) {
	// publish 를 지연시켜 이전 세대의 결과가 버려지는지 확인한다
	run := &fakeRunner{}
	a := newTestApp(t, run)
	var pending []func()
	a.publish = func(fn func()) { pending = append(pending, fn) }
	a.checkAll()
	first := pending
	pending = nil
	a.busy = false // 두 번째 검사를 시작하기 위해 강제로 푼다
	a.checkAll()
	for _, fn := range first {
		fn()
	}
	if !a.busy {
		t.Error("이전 세대 결과가 busy 를 풀면 안 된다")
	}
	for _, fn := range pending {
		fn()
	}
	if a.busy {
		t.Error("현재 세대 결과가 모두 오면 busy 가 풀려야 한다")
	}
}

func TestViewLoadError(t *testing.T) {
	a := newApp(filepath.Join(t.TempDir(), "없는파일"), &fakeRunner{})
	tt := ui.NewTester(a.view, 560, 440)
	if !tt.HasText("다시 읽기") {
		t.Errorf("설정 오류 시 다시 읽기 버튼: %q", tt.Texts())
	}
}

func TestLoginHelpOpensURL(t *testing.T) {
	a := newTestApp(t, &fakeRunner{})
	tt := ui.NewTester(a.view, 560, 440)
	// 로그인 중 상태를 직접 만들어 도움말 UI 를 확인한다
	a.busy = true
	a.loggingIn = "corp"
	a.loginProg = LoginProgress{URL: "https://device.sso.example.com/", Code: "ABCD-1234"}
	tt.Frame()
	if !tt.HasText("ABCD-1234") {
		t.Errorf("코드가 보여야 한다: %q", tt.Texts())
	}
	if err := tt.Click("인증 페이지 열기"); err != nil {
		t.Fatal(err)
	}
	if urls := tt.OpenedURLs(); len(urls) != 1 || urls[0] != "https://device.sso.example.com/" {
		t.Errorf("OpenURL 호출: %v", urls)
	}
	// 로그인이 끝나면 도움말은 사라진다
	a.busy, a.loggingIn, a.loginProg = false, "", LoginProgress{}
	tt.Frame()
	if tt.HasText("인증 페이지 열기") {
		t.Errorf("로그인 후에도 도움말이 남아 있다: %q", tt.Texts())
	}
}

func TestLoadBumpsGeneration(t *testing.T) {
	// 검사 결과가 도착하기 전에 load() 가 실행되면 이전 결과는 버려져야 한다
	a := newTestApp(t, &fakeRunner{})
	var pending []func()
	a.publish = func(fn func()) { pending = append(pending, fn) }
	a.checkAll()
	a.load()
	a.profiles = a.profiles[:1] // 프로필이 줄어든 상황
	for _, fn := range pending {
		fn() // 인덱스 범위를 벗어나는 쓰기가 없어야 한다
	}
	if a.busy || a.profiles[0].status != statusUnknown {
		t.Errorf("busy=%v status=%v", a.busy, a.profiles[0].status)
	}
}
