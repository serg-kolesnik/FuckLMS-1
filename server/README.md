# FuckLMS attendance service

Go-сервис, который хранит студентов в PostgreSQL и выполняет отметку посещаемости
в Moodle LMS ([lms.uni-dubna.ru](https://lms.uni-dubna.ru)) параллельно для списка
студентов. Контракт API — [`../docs/openapi.yaml`](../docs/openapi.yaml), архитектурное
решение — [`../docs/adr/0001-go-attendance-service.md`](../docs/adr/0001-go-attendance-service.md).

## Сборка и запуск

Нужен Go 1.22+ (используются роуты `GET /path` и `{id}` в `net/http`).

```bash
go build ./...        # сборка
go vet ./...          # статический анализ
go build -o fucklms-server .
```

## Запуск через Docker Compose

Стек целиком — PostgreSQL + сервис — описан в [`docker-compose.yml`](docker-compose.yml):

```bash
cp .env.example .env   # заполните TOKEN и POSTGRES_PASSWORD
docker compose up -d
```

- сервис доступен на `http://<хост>:8080`; этот адрес и `TOKEN` вводятся в настройках
  сервиса в Android-приложении;
- схема БД применяется **только при первой инициализации** тома `pgdata`
  (через `docker-entrypoint-initdb.d`); последующие миграции — вручную, как в
  разделе «Миграция»;
- по умолчанию используется образ `ghcr.io/slipynil/fucklms-server:latest`, который
  публикует CI; если образа ещё нет — `docker compose up -d --build` соберёт его
  локально из `Dockerfile`;
- пакет в GHCR по умолчанию приватный: на хосте нужен `docker login ghcr.io`
  (токен с правом `read:packages`) или публичный пакет в настройках GitHub.

## Конфигурация (переменные окружения)

| Переменная | Обязательна | По умолчанию | Описание |
|---|---|---|---|
| `TOKEN` | да | — | bearer-токен для всех ручек, кроме `/health` |
| `DATABASE_URL` | да | — | строка подключения к PostgreSQL (`postgres://user:pass@host:port/db`) |
| `PORT` | нет | `:8080` | адрес HTTP-сервера |
| `MARK_CONCURRENCY` | нет | `5` | сколько студентов отмечать параллельно |
| `LMS_BASE_URL` | нет | `https://lms.uni-dubna.ru` | базовый URL Moodle |

## Миграция

Схема применяется вручную (миграционного фреймворка нет):

```bash
psql "$DATABASE_URL" -f migrations/0001_init.sql
```

## Тесты

Юнит-тесты не требуют ни БД, ни выхода в сеть: Moodle подменяется
`httptest`-заглушкой, хранилище — фейком в памяти.

```bash
go test ./...
```

Интеграционные тесты (build tag `integration`) проверяют `Store` против
настоящего PostgreSQL. Схема пересоздаётся из `migrations/0001_init.sql`,
поэтому база обязана быть **одноразовой** — никаких продакшен-данных:

```bash
FLMS_TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/fucklms_test \
  go test -tags=integration -coverprofile=coverage.out ./...
go tool cover -func=coverage.out   # общее покрытие; гейт CI — 90%
```

## Быстрая проверка

```bash
curl http://localhost:8080/health
# {"database":"up","status":"ok"}

curl -X POST http://localhost:8080/api/students \
  -H 'Authorization: Bearer <TOKEN>' \
  -d '{"firstName":"Иван","lastName":"Иванов","studyGroup":"101-А","lmsLogin":"ivanov","lmsPassword":"secret"}'

# SSE-поток результатов (Ctrl+C — просто закрыть соединение, отметки продолжатся)
curl -N -X POST http://localhost:8080/api/mark \
  -H 'Authorization: Bearer <TOKEN>' \
  -d '{"qrUrl":"https://lms.uni-dubna.ru/mod/attendance/attendance.php?sessid=123&qrpass=abc"}'
```

Без `studentIds` отмечаются все студенты с `isSelected = true`.

## Структура

| Файл | Содержимое |
|---|---|
| `main.go` | сборка: конфиг, пул БД, роуты, мягкое завершение по SIGINT/SIGTERM |
| `config.go` | чтение и валидация переменных окружения |
| `store.go` | доступ к PostgreSQL (`jackc/pgx/v5`), CRUD студентов, валидация |
| `api.go` | HTTP-обработчики: health, CRUD, bearer-авторизация |
| `mark.go` | `POST /api/mark`: разбор QR-ссылки, пул воркеров, SSE-стрим |
| `lms.go` | клиент Moodle: логин (`logintoken`), отметка (`sesskey`, выбор статуса по подписи) |
| `*_test.go` | юнит-тесты + интеграционные (`-tags=integration`), см. раздел «Тесты» |
| `migrations/0001_init.sql` | таблица `students` |

## Безопасность

- `lms_password` хранится в БД **открытым текстом** — осознанное решение ADR 0001.
  Ограничьте доступ к хосту и БД; пароли никогда не возвращаются API.
- Токен сравнивается в постоянном времени (`subtle.ConstantTimeCompare`).
- `qrUrl` принимается только с host'ом `LMS_BASE_URL`.
