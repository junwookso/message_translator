# 서비스 목적

장비 에러 메시지를 로컬 LLM으로 분석해 식별자, 분류, 대상, 신뢰도, 상태를 반환한다.

## 공통 문자열 규칙

모든 문자열 항목은 대문자와 숫자를 사용하고 단어는 언더스코어(`_`)로 구분한다.

## 식별자

메시지에서 불필요한 부분과 변수 값을 제거해 분류를 표현하는 문자열 상수다.

## 카테고리

에러를 큰 범위로 묶는 그룹이다. 예를 들어 오븐 장비의 챔버 1에서 발생한 high temperature 에러의 카테고리는 `HIGH_TEMPERATURE`다.

## 대상 (target)

에러가 발생한 대상이다. 위 예시의 대상은 `CHAMBER_1`이다.

## 상태 (status)

메시지 분석 결과의 처리 상태다.

- `NEW`: 새로 분석한 결과.
- `REUSED`: 모델을 사용하지 않고 기존 작업 결과를 반환한 경우.

## 모델 실행

Gemma 4 E4B IT QAT Q4_0 GGUF를 CPU용 llama.cpp로 실행한다. Compose는 Go API와 모델 두 컨테이너로 구성한다. Go API 내부 어댑터가 OpenAI 호환 API를 호출하며, 모델 파일은 Docker 볼륨에 캐시한다. `MODEL_MODE=remote`가 기본이며 모델 URL이 필수다. 샘플은 `MODEL_MODE=sample`로 명시한다. `APP_ENV`는 dev/test/prod이며 prod에서는 URL 누락과 샘플 모드를 시작 오류로 처리한다.

`api`는 Go API와 모델 호출 어댑터, `model_service`는 모델 이미지와 기본 실행 설정을 관리한다. 루트 Compose는 두 서비스의 연결과 환경변수를 관리한다.

메시지는 UTF-8 기준 최대 2,048바이트다. Go API 단일 인스턴스를 기준으로 MODEL_PARALLEL(기본 2), API_MAX_INFLIGHT(2), API_MAX_QUEUE(4), API_QUEUE_TIMEOUT(5s)을 관리한다. API 동시 호출 수는 모델 슬롯 수 이하여야 한다. 큐 초과는 429, 대기 시간 초과는 503으로 거절한다. 캐시 적중은 슬롯·큐를 사용하지 않는다. 종료 유예는 모델 제한 시간과 큐 대기 시간의 합보다 최소 20초 길게 설정한다.

전체 종료는 scripts/down.sh로 API 종료 후 모델을 중지한다. 기본 엔진은 Podman이며 CONTAINER_ENGINE=docker로 변경한다. 분석 경로의 HEAD 요청은 캐시·모델을 사용하지 않고 405로 거절한다.

## 캐시와 프롬프트

실제 모델의 성공 결과를 SQLite에 저장하며 입력 원문·MODEL_VERSION·PROMPT_VERSION이 일치하면 REUSED로 반환한다. 샘플과 실패 결과는 저장하지 않는다. 캐시 오류는 분석을 막지 않는다.

CACHE_ENABLED는 기본 true이며 false이면 DB 초기화·조회·저장을 모두 생략하고 기존 캐시 파일은 유지한다.

프롬프트는 api/prompts의 버전별 파일을 go:embed로 포함한다. 기존 파일을 유지하고 변경 시 새 버전을 추가한다. 모델이나 프롬프트 변경 시 해당 버전 환경변수도 올린다. API는 Go 1.26 이상을 사용한다.

동일 입력의 진행 중 추론은 캐시 활성화 시 공유한다. 최초 요청은 NEW, 공유 결과는 REUSED다. 요청 하나의 취소는 다른 요청에 영향을 주지 않으며 모두 취소되면 작업을 취소한다. 공유 요청을 포함한 캐시 미적중 접수 수는 API_MAX_INFLIGHT + API_MAX_QUEUE로 제한한다.

모델 이미지는 digest, 모델 파일은 MODEL_REPO·MODEL_REVISION(전체 커밋 SHA)·MODEL_FILE로 고정한다. Compose가 같은 설정을 API와 모델에 전달한다. API는 버전·모델 좌표·이미지·호출 주소·프롬프트와 생성 옵션을 포함한 요청 템플릿의 SHA-256을 캐시 키에 반영한다. SQLite model_version은 MODEL_VERSION과 구성 해시를 결합하며 cache_configurations에 원본 구성을 기록한다. 이전 캐시 데이터는 보존하되 구성 해시 없는 결과는 재사용하지 않는다. 별도 모델 서버 사용 시 해당 서버의 실제 모델/이미지 정보를 환경변수에 맞춰야 한다.
