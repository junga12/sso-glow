package main

import (
	"context"
	"errors"
	"strings"
	"testing"
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

func TestParseProfiles(t *testing.T) {
	got, skipped, err := parseProfiles(strings.NewReader(sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Errorf("건너뛴 프로필이 없어야 한다: %v", skipped)
	}
	want := []Profile{
		{Name: "dev", Session: "corp", Account: "111111111111", Role: "Dev"},
		{Name: "default", Session: "corp", Account: "222222222222", Role: "Admin"},
		{Name: "other-sso", Session: "partner"},
	}
	if len(got) != len(want) {
		t.Fatalf("프로필 %d개, 기대 %d개: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %+v, want %+v", i, got[i], want[i])
		}
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
	got, skipped, err := parseProfiles(strings.NewReader(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Errorf("good 만 남아야 한다: %+v", got)
	}
	want := []string{"-flag", "has space", "ok"}
	if strings.Join(skipped, ",") != strings.Join(want, ",") {
		t.Errorf("skipped %v, want %v", skipped, want)
	}
}

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
