# Go API

Go HTTP API입니다. Go 1.26 이상과 SQLite 드라이버 `modernc.org/sqlite`를 사용합니다.

## 로컬 실행

`api` 디렉터리에서 실행합니다.

```sh
MODEL_MODE=sample go run .
```

기본 포트는 `8080`이며 `PORT` 환경변수로 변경할 수 있습니다.

```sh
PORT=9090 MODEL_MODE=sample go run .
```

## Docker Compose 실행

저장소 루트에서 실행합니다. Go API와 CPU 기반 llama.cpp 모델 컨테이너가 함께 실행됩니다.
API는 모델 컨테이너 시작 후 실행되며 healthcheck 결과를 기다리지 않습니다.
`/ping`은 바로 사용할 수 있습니다. 모델 다운로드·로딩 중 분석 요청은
`502 MODEL_ERROR`를 반환할 수 있으므로 모델 로그의 준비 완료를 확인하고 호출합니다.

```sh
docker compose up --build -d
docker compose logs -f model api
```

전체 종료는 저장소 루트에서 스크립트를 사용합니다(기본 Podman).

```sh
./scripts/down.sh
```

Docker로 실행한 경우 `CONTAINER_ENGINE=docker ./scripts/down.sh`를 사용합니다.
스크립트는 API 종료를 기다린 뒤 전체 서비스를 중지·제거하고 볼륨은 유지합니다.
API 중지 명령이 실패하면 모델을 중지하지 않고 오류로 종료합니다.
일부 Podman Compose 제공자는 전체 서비스를 병렬 중지하므로, 진행 중 분석을
보존하려면 직접 `podman compose down`이나 포그라운드 실행의 Ctrl+C 대신
백그라운드 실행(`-d`)과 이 스크립트를 사용합니다.

## Ping API

```sh
curl -i http://localhost:8080/ping
```

`GET /ping`은 HTTP `200 OK`와 JSON 응답을 반환합니다.

```json
{"message":"pong"}
```

등록되지 않은 경로는 `404`, `/ping`에 대한 지원하지 않는 메서드는 `405`를 반환합니다.
Go의 HTTP 라우터 동작에 따라 `HEAD /ping`도 지원하며 응답 본문은 전송하지 않습니다.

## 장비 메시지 샘플 API

`GET /api/equipment/message`는 `message` 쿼리 파라미터를 받습니다.
`MODEL_MODE=remote`가 기본이며 `MODEL_API_URL`이 필수입니다. 고정 샘플은 `MODEL_MODE=sample`로 명시합니다.
실제 모델의 성공 결과는 SQLite에 저장하며, 같은 입력·모델 버전·프롬프트 버전의 결과를 재사용합니다.

```sh
curl -G http://localhost:8080/api/equipment/message \
  --data-urlencode 'message=오븐 장비의 챔버 1에서 high temperature 에러 발생'
```

HTTP `200 OK` 응답:

```json
{
  "identifier": "CHAMBER_HIGH_TEMPERATURE",
  "category": "HIGH_TEMPERATURE",
  "target": "CHAMBER_1",
  "confidence": 0,
  "status": "NEW"
}
```

식별자는 대상의 번호를 제외한 에러 분류를 표현합니다. `status`는 새 분석 결과를
가정한 `NEW`이며, 실제 분석 전이므로 `confidence`는 `0`인 고정 샘플입니다.

`message`가 없거나 빈 값 또는 공백만 포함하면 HTTP `400 Bad Request`와
`{"error":"MESSAGE_REQUIRED"}`를 반환합니다. 지원하지 않는 메서드는 `405`를 반환합니다.
분석 경로의 `HEAD`도 `405`와 `Allow: GET`을 반환하며, 캐시나 모델을 사용하지 않습니다.

메시지는 URL 디코딩 후 UTF-8 기준 최대 2,048바이트입니다(한글은 일반적으로 글자당 3바이트).
초과하면 모델을 호출하지 않고 HTTP `413`과 `{"error":"MESSAGE_TOO_LONG"}`을 반환합니다.
이 제한은 바이트 수 제한이며 토큰 수를 정확히 계산하는 제한은 아닙니다.
기본값은 API 프로세스당 동시 분석 2건, FIFO 대기열 최대 4건, 슬롯 대기 최대 5초입니다.
캐시 적중과 `/ping`은 대기열·추론 슬롯을 사용하지 않습니다.
대기열이 가득 차면 HTTP `429`와 `{"error":"MODEL_BUSY"}`, 대기 시간이 초과되면
HTTP `503`과 `{"error":"MODEL_QUEUE_TIMEOUT"}`를 반환합니다. 두 응답에 `Retry-After: 1`을
포함합니다. 요청 취소·대기 시간 초과 시 대기 항목을 제거하고, 완료·오류·취소 시 슬롯을 반환합니다.
대기 후 슬롯을 확보하면 캐시를 재조회하므로 앞선 요청이 저장한 결과를 재사용할 수 있습니다.
샘플 모드에도 동일한 요청 제한이 적용됩니다.

## Gemma 4 E4B 모델 연결

기본 모델은 Google의 `gemma-4-E4B-it-qat-q4_0-gguf` 저장소에 있는 `gemma-4-E4B_q4_0-it.gguf`이며, 커밋 리비전을 고정합니다.
공식 CPU 이미지 `compose.yaml`에 고정된 SHA-256 digest로 실행하며, 모델 포트는
Docker 네트워크 안에서만 접근할 수 있습니다. Go API가 직접 프롬프트를 구성합니다.

모델 이미지와 기본 실행 설정은 `model_service`로 분리되어 있습니다.
자세한 내용은 [모델 서비스 문서](../model_service/README.md)를 참고하세요.

- 최초 실행 시 Hugging Face에서 수 GB의 모델 파일을 다운로드합니다. 네트워크와 충분한 디스크·RAM이 필요합니다.
- 모델 파일은 `model-cache` 볼륨에 보관되며 `docker compose down` 후에도 유지됩니다.
- `docker compose down -v`는 캐시도 삭제하므로 다음 실행에서 다시 다운로드합니다.
- 슬롯당 컨텍스트 4096토큰, 기본 동시 추론 2개, CPU 실행, thinking 비활성화, 출력 최대 256토큰을 사용합니다.
- 첫 다운로드·로딩을 위해 healthcheck 유예 시간은 30분입니다. 진행 상황은 모델 로그에서 확인합니다.

설정은 환경변수 또는 저장소 루트의 `.env`로 바꿀 수 있습니다.

| 변수 | 기본값 | 용도 |
| --- | --- | --- |
| `LLAMA_CPP_IMAGE` | `compose.yaml`에 고정된 SHA-256 digest | 모델 Dockerfile의 기반 이미지 빌드 인자, 변경 시 재빌드 |
| `MODEL_REPO` | `google/gemma-4-E4B-it-qat-q4_0-gguf` | GGUF 저장소 |
| `MODEL_REVISION` | `4b4a2c1d584be7264f87aac328a1bc739ce81b6c` | 모델 저장소의 전체 커밋 SHA |
| `MODEL_FILE` | `gemma-4-E4B_q4_0-it.gguf` | 해당 리비전의 GGUF 파일명 |
| `APP_ENV` | `dev` | dev/test/prod |
| `MODEL_MODE` | `remote` | remote/sample; prod에서 sample 금지 |
| `MODEL_API_URL` | Compose: `http://model:8080/v1/chat/completions` | 경로를 포함한 호출 주소 |
| `MODEL_NAME` | `GEMMA_4_E4B` | 요청 모델 이름과 llama.cpp 별칭 |
| `MODEL_VERSION` | `GEMMA_4_E4B_Q4_0_V1` | 캐시 구분용 모델 버전 |
| `PROMPT_VERSION` | `V1` | 포함된 프롬프트 파일 선택 및 캐시 구분 |
| `CACHE_DB_PATH` | 직접 실행: `./data/cache.db`, Compose: `/data/cache.db` | SQLite 파일 경로 |
| `CACHE_ENABLED` | `true` | `false`이면 SQLite 초기화·조회·저장을 모두 건너뜀 |
| `MODEL_TIMEOUT` | `120s` | 양수 Go duration 형식의 모델 호출 제한 시간 |
| `MODEL_PARALLEL` | `2` | 모델 동시 추론 슬롯 수 |
| `API_MAX_INFLIGHT` | `2` | API가 동시에 모델에 보내는 요청 수, `MODEL_PARALLEL` 이하 |
| `API_MAX_QUEUE` | `4` | API 내부 대기 요청 수, `0`이면 즉시 거절 |
| `API_QUEUE_TIMEOUT` | `5s` | 슬롯 확보 최대 대기 시간, 양수 Go duration |
| `API_STOP_GRACE_PERIOD` | Compose: `150s` | 모델 제한 시간 + 큐 대기 시간 + 20초 이상 |

`APP_ENV`는 `dev`(기본), `test`, `prod`를 지원합니다. `prod`에서는 모델 URL 누락이나 샘플 모드로 시작할 수 없습니다.
remote 모드는 환경과 무관하게 올바른 HTTP(S) URL이 필요합니다.
Compose 기본 실행은 실제 모델을 사용합니다. 모델 없이 API 컨테이너만 샘플로 실행하려면:

```sh
APP_ENV=dev MODEL_MODE=sample docker compose up --build --no-deps -d api
```

코드 변경을 반영하려면 저장소 루트에서 다시 빌드합니다.

```sh
docker compose up --build -d
```

모델을 바꾸면서 기존 볼륨을 유지할 수 있으며, 다른 모델 파일이 추가로 캐시됩니다.

### 동시 추론과 대기열 설정

Go API 단일 인스턴스를 기준으로 프로세스 내부에서 대기열을 관리합니다.
아래 네 값은 루트 `.env` 또는 실행 환경변수로 설정합니다.

```env
MODEL_PARALLEL=2
API_MAX_INFLIGHT=2
API_MAX_QUEUE=4
API_QUEUE_TIMEOUT=5s
```

`MODEL_PARALLEL`은 API의 용량 검증과 모델 컨테이너에 함께 전달합니다.
외부 모델을 연결할 때는 실제 서버의 슬롯 수와 맞춰야 합니다.
동시 처리 수는 양수, 대기열 크기는 0 이상이어야 하며 잘못된 설정은 시작 오류입니다.
실제 추론 슬롯 수를 늘리면 CPU·메모리 사용량과 요청별 지연이 증가할 수 있습니다.
모델 시작 스크립트는 총 컨텍스트를 `MODEL_PARALLEL * 4096`으로 설정합니다.

CPU에서 기존 1건 처리·즉시 거절 방식으로 실행하려면:

```sh
MODEL_PARALLEL=1 API_MAX_INFLIGHT=1 API_MAX_QUEUE=0 podman compose up --build -d
```

### 모델 요청과 응답

Go API는 `/v1/chat/completions`에 JSON `POST` 요청을 보냅니다.
장비 분석 규칙은 system 메시지, 입력 원문은 user 메시지로 분리합니다.
`response_format`에 JSON Schema를 지정하고 스트리밍을 끕니다.
`reasoning_effort: none`, `chat_template_kwargs.enable_thinking: false`에 더해
llama.cpp의 `reasoning_budget_tokens: 0`을 지정해 추론 과정이 출력 토큰을 소진하지 않도록 합니다.
기존의 `{ "message": "..." }` 형태로 모델을 직접 호출하던 계약은 더 이상 사용하지 않습니다.

모델 응답의 `choices[0].message.content`에서 아래 네 필드를 파싱합니다.

```json
{
  "identifier": "CHAMBER_HIGH_TEMPERATURE",
  "category": "HIGH_TEMPERATURE",
  "target": "CHAMBER_1",
  "confidence": 0.95
}
```

모든 필드는 필수입니다. 문자열은 비어 있지 않아야 하며 대문자·숫자·언더스코어만
허용합니다. `confidence`는 모델이 반환한 숫자이며 아직 범위를 제한하지 않습니다.
Go API가 새 분석에는 `status: NEW`, 캐시 결과에는 `status: REUSED`를 추가합니다.
실제 분석 결과와 신뢰도는 입력과 모델 출력에 따라 달라집니다.

타임아웃은 HTTP `504`와 `{"error":"MODEL_TIMEOUT"}`, 연결 실패·비정상 HTTP 상태·
잘못된 응답·출력 잘림은 HTTP `502`와 `{"error":"MODEL_ERROR"}`를 반환합니다.
실패 시 샘플로 대체하지 않습니다. API 쓰기 제한 시간은 모델 제한 시간 + 큐 대기 시간 + 10초입니다.

### 캐시와 프롬프트 버전

테스트 시 `CACHE_ENABLED=false`로 캐시를 끌 수 있습니다. 동일한 입력도 매번 모델로
분석하고 성공 시 `NEW`를 반환합니다. 기존 캐시 파일은 삭제하지 않으며 다시 켜면 재사용합니다.
허용값은 `true`, `false`이며 잘못된 값은 시작 오류입니다. 미설정 또는 빈 값은 `true`입니다.

```sh
CACHE_ENABLED=false podman compose up --build -d api
```

직접 실행할 때는 `api` 폴더에서 `CACHE_ENABLED=false MODEL_API_URL=http://localhost:8081/v1/chat/completions go run .`을 사용합니다.

프롬프트는 `prompts/V1.txt` 등 버전별 파일로 관리하고 `go:embed`로 바이너리에 포함합니다.
현재 제공되는 버전은 `V1`입니다. `PROMPT_VERSION=V1`이면 해당 파일을 사용합니다.
기존 파일을 수정하지 않고 `V2.txt`처럼 새 파일을 추가하고 재빌드해 이력을 유지합니다.
버전 이름은 대문자·숫자·언더스코어만 허용하며, 없거나 빈 프롬프트 파일은 시작 오류입니다.

SQLite는 `(message, model_version, prompt_version)`을 복합 키로 사용합니다.
입력 원문을 그대로 비교하므로 공백·대소문자·번호 차이는 별도 입력입니다.
`result_json`에는 네 분석 필드만, `created_at`에는 UTC 생성 시각을 저장합니다.
`status`는 저장하지 않고 요청 시 결정합니다. 샘플·실패 결과는 저장하지 않습니다.
캐시 조회와 저장은 각각 최대 1초이며 모델 추론 동안 DB 연결이나 트랜잭션을 유지하지 않습니다.
DB 초기화·조회·저장 오류 또는 잘못된 저장 결과는 로그를 남기고 캐시 없이 분석을 계속합니다.
캐시 적중은 모델이 분석 중이거나 연결 불가능한 경우에도 재사용할 수 있습니다.

Compose는 `api-data` 볼륨을 `/data`에 연결합니다. 비루트 API 사용자에게 해당 디렉터리의
쓰기 권한을 부여합니다. `podman compose down`은 데이터를 유지하지만
`podman compose down -v`는 API 캐시와 모델 캐시를 모두 삭제합니다.
초기에는 만료·자동 삭제 기능을 두지 않습니다.

모델 변경 시 `MODEL_VERSION`, 프롬프트 변경 시 `PROMPT_VERSION`도 함께 변경합니다.
버전 변경은 이전 결과를 삭제하지 않고 새 캐시 영역을 사용합니다. `MODEL_VERSION`은
실제 모델을 바꾸지 않으며, 모델 선택은 `MODEL_REPO`, `MODEL_REVISION`, `MODEL_FILE`이 담당합니다.

```sh
MODEL_VERSION=GEMMA_4_E4B_Q4_0_V1 PROMPT_VERSION=V1 podman compose up --build -d
```

### 정상 종료

종료 신호를 받으면 새 연결을 받지 않고 진행 중·대기 중인 요청을 기다립니다.
기본 모델 호출 제한 시간은 120초이고, API 큐 대기는 최대 5초입니다.
HTTP 쓰기 제한 시간은 두 시간의 합 + 10초(기본 135초)입니다.
Compose 종료 유예는 기본 150초, 서버 종료 대기는 그보다 10초 짧은 140초입니다.
서버 대기 시간을 초과하면 진행 중·대기 중인 요청의 context를 취소하고 연결을 닫습니다.
정상 종료 시 요청 처리를 마친 뒤 DB를 닫습니다.

직접 Go 서버를 실행할 때 `API_STOP_GRACE_PERIOD`를 생략하면
`MODEL_TIMEOUT + 큐 대기 시간 + 20s`로 계산합니다.
Compose에서는 150초가 지정되므로 타임아웃을 늘릴 때 이 값도 함께 늘려야 합니다.
`API_MAX_QUEUE=0`이면 종료·쓰기 제한 시간 계산에서 큐 대기 시간을 제외합니다.
잘못된 조합은 서버 시작 시 오류로 거절합니다.

```sh
MODEL_TIMEOUT=180s API_STOP_GRACE_PERIOD=210s podman compose up --build -d
```

진행 중 분석을 완료하려면 API의 종료 대기가 끝날 때까지 모델도 실행 중이어야 합니다.
`scripts/down.sh`가 API를 먼저 중지하고 그 명령이 끝난 뒤 전체 서비스를 내립니다.
일반 Compose의 종료 순서나 모델의 healthcheck 실행 여부에 의존하지 않습니다.

참고: [Google E4B GGUF](https://huggingface.co/google/gemma-4-E4B-it-qat-q4_0-gguf),
[llama.cpp 서버](https://github.com/ggml-org/llama.cpp/tree/master/tools/server).

## 테스트

테스트는 `httptest` 모델 서버를 사용하므로 실제 LLM이나 Docker가 필요하지 않습니다.

```sh
go test ./...
go vet ./...
```

컨테이너 실행 후 `/ping`과 장비 메시지 API를 호출해 실제 추론을 확인합니다.
실제 모델 응답은 샘플과 완전히 일치할 필요는 없으며, 필드 형식과 분류 의미를 확인합니다.

종료 통합 테스트는 실제 HTTP API·mock 모델과 Compose 명령 대역을 연결해, 진행 중
분석이 성공한 뒤에만 모델이 종료되는지 확인합니다. Podman/Docker 명령 실패 시
모델을 중지하지 않는 동작과 HEAD 요청의 무분석·무저장 처리도 검사합니다.

Go 빌드·정적 검사와 mock HTTP 서버를 통한 연동 테스트로 검증합니다.
Podman의 비루트 scratch 이미지에서 실제 모델의 첫 응답 `NEW`, 동일 요청의 재응답
`REUSED`와 SQLite 볼륨 쓰기를 확인했습니다. 테스트 컨테이너는 검증 후 정리했습니다.

### 동일 입력의 동시 요청

캐시가 켜져 있으면 동일 입력의 진행 중 추론을 공유합니다. 최초 요청은 `NEW`, 결과를 공유받는 요청은 `REUSED`를 반환합니다. 실패는 저장하지 않습니다. 캐시가 꺼져 있거나 초기화에 실패하면 각 요청이 별도로 추론합니다.

한 요청의 취소는 다른 요청을 취소하지 않습니다. 모든 요청이 취소되면 공유 작업도 취소하고 슬롯을 반환합니다. 공유 요청을 포함한 캐시 미적중 접수 수는 `API_MAX_INFLIGHT + API_MAX_QUEUE` 이하이며, 초과 시 `429 MODEL_BUSY`입니다. 공유 결과 대기는 추론 슬롯을 사용하지 않으며 슬롯 대기 제한 시간과 별개로 해당 작업이 끝날 때까지 기다립니다. 새 추론 작업의 슬롯 대기에는 기존 `API_QUEUE_TIMEOUT`이 적용됩니다.

### 모델 구성과 캐시 연결

모델 이미지의 SHA-256 digest와 GGUF 저장소의 전체 커밋 SHA를 고정합니다. Compose가 같은 모델 좌표와 이미지 정보를 두 서비스에 전달합니다. remote 모드에서 가변 이미지 태그나 `main` 같은 모델 리비전은 허용하지 않습니다. 모델 이미지를 바꿀 때는 API와 모델을 함께 재빌드·재시작합니다.

캐시에는 버전 이름 외에 모델 저장소·리비전·파일·이미지·호출 주소·요청 모델명·프롬프트 원문·JSON 스키마·생성 옵션의 SHA-256을 반영합니다. SQLite의 `model_version`은 `MODEL_VERSION_<구성 해시>`로 저장하고 `cache_configurations` 테이블에 구성 JSON을 기록합니다. 프롬프트 내용을 실수로 같은 버전에서 바꿔도 이전 캐시를 재사용하지 않습니다.

기존 DB와 결과는 보존됩니다. 구성 해시가 없는 이전 결과는 새 구성에서 재사용하지 않으므로 업데이트 후 첫 요청은 새로 분석합니다. 만료·자동 정리는 추가하지 않습니다.

별도로 운영하는 모델 서버에 연결할 때도 `MODEL_REPO`, `MODEL_REVISION`, `MODEL_FILE`, `LLAMA_CPP_IMAGE`를 실제 서버 구성에 맞춰야 합니다. API는 원격 서버의 실행 바이너리나 모델 파일을 직접 검증하지 않습니다.
