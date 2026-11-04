package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	ssooidctypes "github.com/aws/aws-sdk-go-v2/service/ssooidc/types"
	"github.com/aws/smithy-go"
)

const sampleConfig = `
[sso-session corp]
sso_region = ap-northeast-1
sso_start_url = https://example.awsapps.com/start

[profile dev]
region = ap-northeast-1
sso_session = corp
sso_account_id = 111111111111
sso_role_name = Dev

[profile legacy]
region = ap-northeast-1
aws_access_key_id = AKIA_NOT_SSO

[default]
sso_session=corp
sso_account_id=222222222222
sso_role_name=Admin

; 주석
[profile other-sso]
sso_session = partner
`

func TestParseConfig(t *testing.T) {
	cfg, err := parseConfig(strings.NewReader(sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Skipped) != 0 {
		t.Errorf("건너뛴 프로필이 없어야 한다: %v", cfg.Skipped)
	}
	want := []Profile{
		{Name: "dev", Session: "corp", Account: "111111111111", Role: "Dev", Region: "ap-northeast-1"},
		{Name: "default", Session: "corp", Account: "222222222222", Role: "Admin"},
		{Name: "other-sso", Session: "partner"},
	}
	got := cfg.Profiles
	if len(got) != len(want) {
		t.Fatalf("프로필 %d개, 기대 %d개: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %+v, want %+v", i, got[i], want[i])
		}
	}
	if s := cfg.Sessions["corp"]; s != (Session{Name: "corp", Region: "ap-northeast-1"}) {
		t.Errorf("sso-session corp: %+v", s)
	}
	if _, ok := cfg.Sessions["partner"]; ok {
		t.Errorf("정의되지 않은 세션은 없어야 한다: %+v", cfg.Sessions)
	}
}

func TestSessionsOf(t *testing.T) {
	ps := []Profile{{Session: "a"}, {Session: "b"}, {Session: "a"}, {Session: ""}}
	got := sessionsOf(ps)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("got %v", got)
	}
}

func TestCLIError(t *testing.T) {
	err := cliError(context.Background(), errors.New("exit status 255"),
		"\naws: [ERROR]: Error when retrieving token from sso: Token has expired and refresh failed\n상세 줄\n")
	if err.Error() != "Token has expired and refresh failed" {
		t.Errorf("got %q", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()
	if err := cliError(ctx, errors.New("signal: killed"), ""); err.Error() != "시간 초과" {
		t.Errorf("타임아웃이면 시간 초과: %q", err)
	}
	if err := cliError(context.Background(), errors.New("boom"), ""); !strings.Contains(err.Error(), "boom") {
		t.Errorf("stderr 가 비면 원래 에러를 담아야 한다: %q", err)
	}
}

func TestProfileDetail(t *testing.T) {
	cases := []struct {
		in   Profile
		want string
	}{
		{Profile{Account: "1", Role: "Dev", Session: "corp"}, "1 / Dev  ·  corp"},
		{Profile{Session: "partner"}, "partner"},
		{Profile{Account: "1", Session: "corp"}, "1  ·  corp"},
		{Profile{}, ""},
	}
	for _, c := range cases {
		if got := profileDetail(c.in); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseProfilesSkipsUnsafeNames(t *testing.T) {
	cfg := `
[profile -flag]
sso_session = corp

[profile has space]
sso_session = corp

[profile ok]
sso_session = -bad

[profile good]
sso_session = corp
`
	c, err := parseConfig(strings.NewReader(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Profiles; len(got) != 1 || got[0].Name != "good" {
		t.Errorf("good 만 남아야 한다: %+v", got)
	}
	want := []string{"-flag", "has space", "ok"}
	if strings.Join(c.Skipped, ",") != strings.Join(want, ",") {
		t.Errorf("skipped %v, want %v", c.Skipped, want)
	}
}

func TestSDKError(t *testing.T) {
	ctx := context.Background()
	// SDK 가 실제로 만드는 래핑 순서를 흉내낸다
	grant := fmt.Errorf("refresh cached SSO token failed, %w",
		fmt.Errorf("unable to refresh SSO token, %w",
			&smithy.OperationError{ServiceID: "SSO OIDC", OperationName: "CreateToken",
				Err: &ssooidctypes.InvalidGrantException{Message: strPtr("Invalid grant provided")}}))
	cases := []struct {
		in   error
		want string
	}{
		{nil, ""},
		{grant, "SSO 로그인 세션이 끝났습니다. 다시 로그인하세요"},
		{&ssocreds.InvalidTokenError{}, "SSO 토큰이 만료되었습니다. 다시 로그인하세요"},
		{errors.New("refresh cached SSO token failed, cached SSO token is expired, or not present, and cannot be refreshed"),
			"SSO 토큰이 만료되었습니다. 다시 로그인하세요"},
		{fmt.Errorf("failed to read cached SSO token file, %w", fs.ErrNotExist), "SSO 로그인 기록이 없습니다. 로그인하세요"},
		{&smithy.GenericAPIError{Code: "UnauthorizedException", Message: "Session token not found or invalid"},
			"UnauthorizedException: Session token not found or invalid"},
		{errors.New(strings.Repeat("x", 200)), strings.Repeat("x", 160) + "…"},
	}
	for _, c := range cases {
		got := ""
		if err := sdkError(ctx, c.in); err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%v:\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	tctx, cancel := context.WithTimeout(ctx, 0)
	defer cancel()
	<-tctx.Done()
	if err := sdkError(tctx, errors.New("context deadline exceeded")); err.Error() != "시간 초과" {
		t.Errorf("타임아웃: %q", err)
	}
}

func strPtr(s string) *string { return &s }

func TestParseLoginLine(t *testing.T) {
	var p LoginProgress
	lines := []string{
		"Attempting to automatically open the SSO authorization page in your default browser.",
		"If the browser does not open or you wish to use a different device to authorize this request, open the following URL:",
		"",
		"https://device.sso.ap-northeast-1.amazonaws.com/",
		"",
		"Then enter the code:",
		"",
		"ABCD-1234",
	}
	hits := 0
	for _, l := range lines {
		if parseLoginLine(l, &p) {
			hits++
		}
	}
	if hits != 2 || p.URL != "https://device.sso.ap-northeast-1.amazonaws.com/" || p.Code != "ABCD-1234" {
		t.Errorf("hits=%d prog=%+v", hits, p)
	}
	// PKCE 방식은 코드 없이 긴 URL 만 나온다
	var q LoginProgress
	if !parseLoginLine("  https://oidc.ap-northeast-1.amazonaws.com/authorize?x=1&y=2  ", &q) || q.URL != "https://oidc.ap-northeast-1.amazonaws.com/authorize?x=1&y=2" {
		t.Errorf("PKCE URL: %+v", q)
	}
}
