# 모델 서비스

Gemma 4 E4B IT QAT Q4_0 GGUF를 CPU용 llama.cpp 서버로 실행합니다.
Dockerfile에서 기본 모델, 실행 옵션, healthcheck를 관리합니다.
Go API의 프롬프트 구성·응답 변환은 `api`에 있습니다.

## Compose 실행

저장소 루트에서 실행합니다.

```sh
docker compose up --build -d
docker compose logs -f model api
```

최초 실행 시 모델을 다운로드하고 `model-cache` 볼륨에 보관합니다.
전체 종료는 루트의 `./scripts/down.sh`를 사용합니다(기본 Podman).
Docker는 `CONTAINER_ENGINE=docker ./scripts/down.sh`를 사용합니다.
API 종료를 기다린 뒤 모델을 중지하므로 진행 중 분석을 완료할 수 있습니다.
스크립트는 캐시 볼륨을 유지하며, 별도로 `compose down -v`를 실행하면 삭제됩니다.
API는 모델 컨테이너 시작 후 바로 실행합니다. 모델 포트는 외부에 공개하지 않습니다.
모델 준비 전에도 `/ping`은 동작하며, 분석 요청은 `502 MODEL_ERROR`를 반환할 수 있습니다.

API에서 사용하는 모델 주소는 `http://model:8080/v1/chat/completions`입니다.

## 설정

Compose는 환경변수 또는 루트 `.env`의 설정을 전달합니다.

| 변수 | 기본값 | 적용 |
| --- | --- | --- |
| `LLAMA_CPP_IMAGE` | `compose.yaml`의 고정 SHA-256 digest | Docker 빌드 인자, 기반 이미지 |
| `MODEL_REPO` | `google/gemma-4-E4B-it-qat-q4_0-gguf` | 모델 저장소 |
| `MODEL_REVISION` | `4b4a2c1d584be7264f87aac328a1bc739ce81b6c` | 전체 커밋 SHA |
| `MODEL_FILE` | `gemma-4-E4B_q4_0-it.gguf` | GGUF 파일명 |
| `MODEL_PARALLEL` | `2` | 모델 추론 슬롯 수, API 설정에도 전달 |
| `MODEL_NAME` | `GEMMA_4_E4B` | `LLAMA_ARG_ALIAS`, API 요청과 일치하는 모델 별칭 |

슬롯당 컨텍스트는 4096토큰, 동시 추론은 기본 2개이며 GPU와 thinking은 사용하지 않습니다.
`start.sh`에서 `MODEL_PARALLEL`을 검증하고 총 컨텍스트를 `MODEL_PARALLEL * 4096`으로
계산한 뒤 `exec`로 llama.cpp를 실행합니다. 종료 신호는 모델 프로세스에 직접 전달됩니다.
`--reasoning-budget 0`으로 추론 토큰 예산도 제한합니다. API 요청에서도
`reasoning_budget_tokens: 0`을 지정합니다. 비활성화 옵션만으로는 모델이 추론 과정을
생성해 256토큰 제한에 도달하고 최종 JSON이 비어 있는 경우가 있어 두 설정을 함께 사용합니다.
텍스트만 처리하므로 멀티모달 projector는 다운로드하지 않습니다.
healthcheck는 10초 간격, 5초 제한, 30분 시작 유예, 6회 실패 기준입니다.
Podman의 OCI 이미지 빌드에서는 Dockerfile의 HEALTHCHECK가 생략될 수 있어,
Compose에도 healthcheck를 명시합니다. Podman에서는 `podman compose up --build -d`로 실행합니다.
Podman에서 자동 healthcheck가 실행되지 않아도 API 시작을 막지 않도록 Compose의
시작 조건은 `service_started`를 사용합니다. 모델 상태를 즉시 확인하려면
`podman healthcheck run <모델 컨테이너 이름>`으로 수동 검사할 수 있습니다.
출력 토큰 수와 모델 호출 제한 시간은 Go API에서 관리합니다.

기반 이미지나 Dockerfile 변경 후에는 다시 빌드합니다.

```sh
docker compose up --build -d
```

## 단독 실행

저장소 루트에서 실행합니다. 호스트 테스트 포트는 `8081`입니다.

```sh
docker build -t sig-model ./model_service
docker run --name sig-model --rm -p 127.0.0.1:8081:8080 -v sig-model-cache:/models sig-model
```

이 경우 캐시는 Compose와 별도의 `sig-model-cache` 볼륨에 저장됩니다.
호스트에서 실행하는 Go API는 아래처럼 연결합니다(`api` 디렉터리에서 실행).

```sh
MODEL_API_URL=http://localhost:8081/v1/chat/completions go run .
```

Podman에서 모델을 실행하고 `reasoning_budget_tokens: 0` 요청의 실제 JSON 응답을 확인했습니다.

### 고정된 모델 파일

`start.sh`는 `https://huggingface.co/$MODEL_REPO/resolve/$MODEL_REVISION/$MODEL_FILE`에서 파일을 받으며 `/models/$MODEL_REPO/$MODEL_REVISION/$MODEL_FILE`에 보관합니다. 저장소나 리비전이 달라지면 다른 파일 경로를 사용합니다. 기존 자동 다운로드 경로의 파일은 삭제하지 않으며, 이 변경 이후 처음 실행할 때 고정 경로로 다시 다운로드할 수 있습니다.

Compose의 모델 구성은 API 캐시 구분에도 전달됩니다. 모델 좌표·이미지 변경 후에는 API와 모델을 함께 재시작합니다. 모델 이미지 digest는 `LLAMA_CPP_IMAGE`, 모델 파일은 리비전과 파일명으로 명시합니다. `MODEL_REPO`에는 기존 `:Q4_0` 접미사를 넣지 않습니다.
