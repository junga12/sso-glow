# sso-glow

AWS SSO 세션이 살아 있는지 한눈에 확인하고, 만료됐으면 버튼 하나로 다시 로그인하는 macOS 데스크톱 앱입니다.
[egoist/mygo](https://github.com/egoist/mygo) 네이티브 UI로 만들어 웹뷰나 Node 런타임 없이 Go 바이너리 하나로 동작합니다.

## 동작

1. `~/.aws/config`(또는 `AWS_CONFIG_FILE`)에서 `sso_session`이 설정된 프로필과 `sso-session` 정의를 읽습니다.
2. 창이 뜨면 `sso-session`마다 로그인 세션이 살아 있는지 검사합니다. 캐시된 액세스 토큰이 만료됐으면 리프레시 토큰으로 갱신을 시도하고, Identity Center가 거절하면(포털에서 로그아웃했거나 관리자가 세션을 끊은 경우) 그 세션을 쓰는 프로필을 모두 **만료**로 표시합니다. 살아 있는 세션의 프로필은 병렬로 `GetCallerIdentity`를 호출해 역할 자격증명까지 확인합니다.
3. 만료된 프로필이 있으면 **SSO 로그인** 버튼이 활성화됩니다. 누르면 만료된 프로필들이 참조하는 `sso-session`마다 한 번씩 `aws sso login --sso-session <이름>`을 실행합니다. 프로필이 여러 개라도 같은 세션을 쓰면 브라우저 인증은 한 번만 뜹니다.
4. 로그인이 끝나면 자동으로 다시 검사합니다.

브라우저가 자동으로 열리지 않으면 aws CLI가 출력한 인증 URL과 코드를 화면에 보여주고, **인증 페이지 열기** 버튼으로 직접 열 수 있습니다.

## 기술 스택

| 구분 | 내용 |
|---|---|
| 언어 | Go 1.27 (cgo 미사용, 외부 런타임 없음) |
| UI 프레임워크 | [egoist/mygo](https://github.com/egoist/mygo) v0.7.4 네이티브 UI. 웹뷰 없이 Go 코드로 뷰를 선언하면 Metal로 직접 그립니다. 플렉스박스 레이아웃, 시스템 테마(라이트·다크) 자동 적용 |
| AWS 연동 | 검사는 AWS SDK for Go v2(`ssocreds`, `sts`)로, 로그인은 AWS CLI v2를 `os/exec`로 호출(`sso login`). CLI는 역할 자격증명을 `~/.aws/cli/cache`에 디스크 캐시로 두기 때문에 SSO 로그인 세션이 끝난 뒤에도 한동안 성공하지만, SDK는 매번 SSO 토큰 경로를 타므로 다른 SDK 기반 도구와 같은 판정을 냅니다 |
| 동시성 | 세션 검사는 순차(동시 토큰 갱신 시 리프레시 토큰이 교체되어 뒤의 요청이 거절됨), 프로필 검사는 고루틴으로 병렬. 결과는 `Window.Update`로 메인 스레드에 반영 |
| 테스트 | 표준 `testing` + mygo의 `ui.NewTester`(창 없이 뷰를 렌더링해 클릭·텍스트 검증). aws CLI는 인터페이스로 분리해 가짜 구현으로 대체 |
| 빌드·배포 | `mygo` CLI로 `.app` 번들과 DMG 생성, ad hoc 코드 서명 |
| 의존성 | `github.com/egoist/mygo`와 `github.com/aws/aws-sdk-go-v2`(config, credentials, sts, ssooidc). 모두 순수 Go |

## 요구 사항

- macOS 12 이상 (Apple Silicon 기준으로 빌드. Intel은 아래 빌드 옵션 참고)
- [AWS CLI v2](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html). `aws` 실행 파일을 `PATH`, `/opt/homebrew/bin`, `/usr/local/bin`, `/usr/bin` 순으로 찾습니다.
- `~/.aws/config`에 `sso-session`과 그것을 참조하는 프로필

```ini
[sso-session my-sso]
sso_start_url = https://example.awsapps.com/start
sso_region = ap-northeast-1
sso_registration_scopes = sso:account:access

[profile dev]
sso_session = my-sso
sso_account_id = 123456789012
sso_role_name = Developer
region = ap-northeast-1
```

빌드하려면 Go 1.27.3 이상이 필요합니다. 더 낮은 버전이라도 `GOTOOLCHAIN=auto`(기본값)이면 자동으로 내려받습니다.

## 빌드와 설치

```sh
# 개발 실행 (라이브 리로드)
go tool mygo dev

# 앱 번들과 DMG 생성 → build/darwin-arm64/
go tool mygo build -skip-notarize

# Intel Mac용도 함께 만들 때
go tool mygo build -skip-notarize -platform darwin/arm64,darwin/amd64

# 설치
ditto build/darwin-arm64/sso-glow.app /Applications/sso-glow.app
```

`-skip-notarize`를 빼면 Apple Developer ID 서명과 공증을 시도합니다. 설정은 `mygo.json`의 `macos.signingIdentity`, `macos.notarize`에 둡니다. ad hoc 서명으로 빌드한 앱은 빌드한 Mac에서만 경고 없이 열립니다.

## 테스트

```sh
go tool mygo vet .                 # 빌드 값이 상태나 고루틴에 새지 않는지 검사
CGO_ENABLED=0 go test -race ./...  # 설정 파싱, 로그인 출력 파싱, UI 동작
```

UI 테스트는 `ui.NewTester`로 창 없이 실행되며, aws CLI는 가짜 runner로 대체합니다.

## 구조

| 파일 | 역할 |
|---|---|
| `sso.go` | config 파싱, SDK 세션·프로필 검사와 에러 정리, aws CLI 로그인 실행과 출력에서 URL·코드 추출 |
| `main.go` | 앱 상태, 뷰, 백그라운드 결과를 메인 스레드에 반영하는 흐름 |
| `sso_test.go` | 파싱과 에러 메시지 정리 테스트 |
| `main_test.go` | 검사 → 로그인 → 재검사 등 UI 시나리오 테스트 |
| `mygo.json` | 앱 이름, 번들 식별자, 버전, 출력 경로 |

## 보안 메모

- 외부 명령은 셸을 거치지 않고 인자 배열로 실행합니다. config의 프로필·세션 이름이 `-`로 시작하거나 공백을 포함하면 aws 인자로 넘기지 않고 건너뜁니다.
- `~/.aws/sso/cache`의 토큰 파일은 AWS SDK가 읽고, 만료됐으면 갱신해 같은 파일에 같은 권한으로 다시 씁니다. 앱 코드는 토큰 값을 받지 않고 버리며, 화면·로그에 쓰지 않습니다. 리프레시 토큰과 클라이언트 시크릿은 SDK가 `sso_region`의 Identity Center OIDC 엔드포인트에 TLS로만 보냅니다.
- `~/.aws/credentials`는 읽지 않도록 SDK에 빈 목록을 넘기고, EC2 메타데이터(IMDS) 조회도 끕니다. 역할 자격증명은 프로세스 메모리에만 두고 디스크에 캐시하지 않습니다.
- aws CLI의 출력은 저장하거나 로그로 남기지 않습니다. 오류는 SDK 에러 코드와 메시지 또는 CLI stderr 첫 줄을 160자까지만 화면에 표시합니다.
- 화면에는 config에 적힌 계정 ID와 역할명이 표시됩니다.
