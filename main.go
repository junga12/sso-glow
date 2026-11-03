package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// status 는 프로필 하나의 검사 결과다.
type status int

const (
	statusUnknown  status = iota // 아직 검사하지 않음
	statusChecking               // 검사 중
	statusValid                  // 세션 유효
	statusInvalid                // 세션 만료 또는 오류
)

// profileState 는 프로필과 그 검사 결과를 묶는다.
type profileState struct {
	Profile
	status status
	err    string // statusInvalid 일 때 사용자에게 보여줄 한 줄
}

// app 은 창이 보여주는 상태다. 뷰는 메인 스레드에서만 이 상태를 읽고,
// 백그라운드 작업은 publish 를 통해서만 상태를 바꾼다.
type app struct {
	configPath string
	profiles   []profileState
	loadErr    string

	busy      bool          // 검사 또는 로그인 진행 중
	loggingIn string        // 로그인 중인 sso-session 이름. 비어 있으면 로그인 중이 아님
	loginProg LoginProgress // 로그인 중 aws 가 알려준 인증 URL 과 코드
	message   string        // 하단 상태 줄
	gen       int           // 검사 세대. 이전 세대의 결과는 버린다

	run     runner
	publish func(func()) // 메인 스레드에서 fn 을 실행하고 다시 그린다 (Window.Update)
	spawn   func(func()) // 백그라운드 실행. 기본은 go, 테스트는 동기 실행
}

func newApp(configPath string, run runner) *app {
	a := &app{
		configPath: configPath,
		run:        run,
		publish:    func(fn func()) { fn() },
		spawn:      func(fn func()) { go fn() },
	}
	a.load()
	return a
}

// load 는 config 를 다시 읽어 프로필 목록을 만든다. 검사 결과는 초기화되고,
// 진행 중이던 검사의 결과는 세대가 바뀌어 버려진다.
func (a *app) load() {
	a.gen++
	a.busy = false
	a.loggingIn = ""
	a.loginProg = LoginProgress{}
	a.profiles = nil
	a.loadErr = ""
	a.message = ""
	profiles, skipped, err := loadProfiles(a.configPath)
	if err != nil {
		a.loadErr = fmt.Sprintf("설정을 읽을 수 없습니다: %v", err)
		return
	}
	if len(skipped) > 0 {
		a.message = fmt.Sprintf("건너뜀 (이름에 허용되지 않는 문자): %s", strings.Join(skipped, ", "))
	}
	if len(profiles) == 0 {
		a.loadErr = "sso_session 이 설정된 프로필이 없습니다: " + a.configPath
		return
	}
	for _, p := range profiles {
		a.profiles = append(a.profiles, profileState{Profile: p})
	}
}

// invalid 는 만료로 판정된 프로필들을 돌려준다.
func (a *app) invalid() []Profile {
	var out []Profile
	for _, p := range a.profiles {
		if p.status == statusInvalid {
			out = append(out, p.Profile)
		}
	}
	return out
}

// checkAll 은 모든 프로필을 병렬로 검사한다. 결과는 publish 로 메인 스레드에 반영한다.
func (a *app) checkAll() {
	if a.busy || len(a.profiles) == 0 {
		return
	}
	a.busy = true
	a.gen++
	a.message = "세션을 확인하는 중…"
	gen := a.gen
	remaining := len(a.profiles)
	for i := range a.profiles {
		a.profiles[i].status = statusChecking
		a.profiles[i].err = ""
		i := i
		name := a.profiles[i].Name
		a.spawn(func() {
			err := a.run.Check(context.Background(), name)
			a.publish(func() {
				if gen != a.gen {
					return // 그 사이 다시 확인이 시작됐다
				}
				if err == nil {
					a.profiles[i].status = statusValid
				} else {
					a.profiles[i].status = statusInvalid
					a.profiles[i].err = err.Error()
				}
				remaining--
				if remaining == 0 {
					a.busy = false
					a.message = a.summary()
				}
			})
		})
	}
}

// summary 는 검사 완료 후 하단에 보여줄 한 줄이다.
func (a *app) summary() string {
	n := len(a.invalid())
	if n == 0 {
		return fmt.Sprintf("모든 AWS SSO 세션이 유효합니다. (%d개 프로필)", len(a.profiles))
	}
	return fmt.Sprintf("%d개 프로필 중 %d개의 세션이 유효하지 않습니다.", len(a.profiles), n)
}

// login 은 만료된 프로필들이 쓰는 sso-session 마다 한 번씩 로그인하고, 끝나면 다시 검사한다.
func (a *app) login() {
	if a.busy {
		return
	}
	sessions := sessionsOf(a.invalid())
	if len(sessions) == 0 {
		return
	}
	a.busy = true
	gen := a.gen
	a.spawn(func() {
		for _, s := range sessions {
			a.publish(func() {
				a.loggingIn = s
				a.loginProg = LoginProgress{}
				a.message = fmt.Sprintf("SSO 세션(%s) 로그인 중… 브라우저에서 인증을 완료하세요.", s)
			})
			progress := func(p LoginProgress) {
				a.publish(func() {
					if gen == a.gen && a.loggingIn == s {
						a.loginProg = p
					}
				})
			}
			if err := a.run.Login(context.Background(), s, progress); err != nil {
				a.publish(func() {
					if gen != a.gen {
						return
					}
					a.busy = false
					a.loggingIn = ""
					a.loginProg = LoginProgress{}
					a.message = fmt.Sprintf("SSO 세션(%s) 로그인 실패: %v", s, err)
				})
				return
			}
		}
		a.publish(func() {
			if gen != a.gen {
				return
			}
			a.busy = false
			a.loggingIn = ""
			a.loginProg = LoginProgress{}
			a.checkAll()
		})
	})
}

func (a *app) view(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Fill().Padding(20).Gap(14).Children(func() {
		// 헤더
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "AWS SSO 세션").FontSize(22).Bold()
			ui.Spacer(c)
			if a.busy {
				ui.Spinner(c).Label("진행 중")
			}
		})
		ui.Text(c, a.configPath).TextColor(t.TextMuted).FontSize(12).SingleLine()

		if a.loadErr != "" {
			ui.Text(c, a.loadErr).TextColor(t.Danger).Selectable()
		} else {
			// 프로필 목록
			ui.Scroll(c).Grow(1).Children(func() {
				ui.Column(c).Gap(6).Children(func() {
					for i := range a.profiles {
						a.profileRow(c, &a.profiles[i])
					}
				})
			})
		}

		// 상태 줄
		if a.message != "" {
			ui.Text(c, a.message).TextColor(t.TextMuted).FontSize(13).Selectable()
		}
		a.loginHelp(c)

		// 동작 버튼
		ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
			if a.loadErr != "" {
				if ui.Button(c, "다시 읽기").Clicked() {
					a.load()
				}
				return
			}
			ui.Button(c, "다시 확인").Disabled(a.busy).OnClick(a.checkAll)
			n := len(sessionsOf(a.invalid()))
			label := "SSO 로그인"
			if n > 1 {
				label = fmt.Sprintf("SSO 로그인 (%d개 세션)", n)
			}
			ui.PrimaryButton(c, label).Disabled(a.busy || n == 0).OnClick(a.login)
		})
	})
}

// loginHelp 는 로그인 중 브라우저가 열리지 않았을 때를 위해 인증 URL 과 코드를 보여준다.
func (a *app) loginHelp(c *ui.Context) {
	if a.loggingIn == "" || a.loginProg.URL == "" {
		return
	}
	t := c.Theme()
	url := a.loginProg.URL
	svc := c.Services()
	ui.Row(c).Gap(10).AlignItems(ui.Center).Wrap().Children(func() {
		ui.Text(c, "브라우저가 열리지 않으면:").TextColor(t.TextMuted).FontSize(13)
		ui.Button(c, "인증 페이지 열기").OnClick(func() { svc.OpenURL(url) })
		if code := a.loginProg.Code; code != "" {
			ui.Text(c, "코드").TextColor(t.TextMuted).FontSize(13)
			ui.Text(c, code).Font("monospace").Bold().Selectable()
		}
	})
}

// profileRow 는 프로필 한 줄을 그린다: 상태 배지, 이름, 계정/역할, 오류.
func (a *app) profileRow(c *ui.Context, p *profileState) {
	t := c.Theme()
	ui.Row(c.Key(p.Name)).Gap(12).AlignItems(ui.Center).Padding(10, 12).
		Background(t.Surface).Radius(t.Radius).Children(func() {
		a.statusBadge(c, p)
		ui.Column(c).Gap(2).Grow(1).Children(func() {
			ui.Text(c, p.Name).Bold().SingleLine()
			ui.Text(c, profileDetail(p.Profile)).TextColor(t.TextMuted).FontSize(12).SingleLine()
			if p.status == statusInvalid && p.err != "" {
				ui.Text(c, p.err).TextColor(t.Danger).FontSize(12).SingleLine()
			}
		})
	})
}

// profileDetail 은 "계정 / 역할  ·  세션" 형식으로, 비어 있는 항목은 건너뛴다.
func profileDetail(p Profile) string {
	var parts []string
	if p.Account != "" {
		parts = append(parts, p.Account)
	}
	if p.Role != "" {
		parts = append(parts, p.Role)
	}
	detail := strings.Join(parts, " / ")
	if p.Session != "" {
		if detail != "" {
			detail += "  ·  "
		}
		detail += p.Session
	}
	return detail
}

func (a *app) statusBadge(c *ui.Context, p *profileState) {
	t := c.Theme()
	switch p.status {
	case statusValid:
		ui.Badge(c, "유효").Background(t.Success).TextColor(t.AccentText)
	case statusInvalid:
		ui.Badge(c, "만료").Background(t.Danger).TextColor(t.AccentText)
	case statusChecking:
		ui.Badge(c, "확인 중").Background(t.Warning).TextColor(t.AccentText)
	default:
		ui.Badge(c, "미확인")
	}
}

func main() {
	run, err := newAWSRunner()
	if err != nil {
		log.Fatal(err)
	}
	a := newApp(defaultConfigPath(), run)
	mygo.App.WhenReady(func() {
		win := mygo.NewWindow(mygo.WindowOptions{
			Title:     "sso-glow",
			Width:     560,
			Height:    440,
			MinWidth:  420,
			MinHeight: 320,
			StateKey:  "main",
			Content:   ui.View(a.view),
		})
		// 백그라운드 결과는 메인 스레드에서 반영한 뒤 다시 그린다.
		a.publish = win.Update
		// 창이 뜨면 바로 검사한다.
		a.checkAll()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
