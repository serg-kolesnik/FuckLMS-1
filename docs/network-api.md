# Сетевой слой и модель данных FuckLMS

> Документ описывает **целевую архитектуру** (ADR 0001): сетевое взаимодействие с Moodle LMS
> вынесено в отдельный Go-сервис. Android-приложение общается только с сервисом; сервис —
> с PostgreSQL и с Moodle (`https://lms.uni-dubna.ru`). Машин-readable контракт API сервиса —
> в [`openapi.yaml`](openapi.yaml).

## Обзор

| Связь | Технология | Суть |
|---|---|---|
| Приложение → сервис | JSON, SSE, bearer-токен | CRUD студентов, запуск отметки, поток результатов |
| Сервис → PostgreSQL | SQL (`jackc/pgx`) | Таблица `students` — единственное хранилище аккаунтов |
| Сервис → Moodle | HTTP-формы + cookies | Логин и отметка посещаемости, параллельно на каждого студента |

По решению ADR 0001 удаляются прежние механизмы: WebView-автоматизация (`BrowserScreen`
с JS-инъекциями), вспомогательный `MoodleClient.kt` и локальное хранилище аккаунтов
(`AccountManager`, ключ `accounts_list` в `EncryptedSharedPreferences`).

---

## 1. Хранилище: PostgreSQL

Таблица `students` (миграция `server/migrations/0001_init.sql`):

```sql
CREATE TABLE students (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    first_name   VARCHAR(100) NOT NULL,
    last_name    VARCHAR(100) NOT NULL,
    study_group  VARCHAR(50)  NOT NULL,
    lms_login    VARCHAR(100) NOT NULL UNIQUE,
    lms_password VARCHAR(255) NOT NULL,
    is_selected  BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_students_group ON students (study_group);
```

Ключевые решения:

- `study_group`, а не `group` — `GROUP` зарезервированное слово SQL; индекс — для
  фильтра «отметить всю группу»;
- `UNIQUE` на `lms_login` — дубликаты логинов отсекаются на уровне БД
  (раньше это делал `AccountManager`);
- `is_selected` — аналог галочки «использовать аккаунт»: отметка без явного списка
  студентов берёт всех с `is_selected = true`;
- `lms_password` хранится **открытым текстом** — осознанное решение ADR 0001
  (сервису нужен исходный пароль для логина в Moodle; компенсации — bearer-токен
  на API и ограничение доступа к БД на уровне хоста).

---

## 2. API сервиса

Авторизация: все ручки, кроме `GET /health`, требуют заголовок
`Authorization: Bearer <token>`; без него — `401`.

### `GET /api/students` — список студентов

Ответ — массив объектов. **Пароли не возвращаются никогда**:

```json
{
  "id": 1,
  "firstName": "Иван",
  "lastName": "Иванов",
  "studyGroup": "101-А",
  "lmsLogin": "ivanov",
  "isSelected": true,
  "createdAt": "2026-11-08T12:00:00Z",
  "updatedAt": "2026-11-08T12:00:00Z"
}
```

### `POST /api/students` — создать студента

Тело: `firstName`, `lastName`, `studyGroup`, `lmsLogin`, `lmsPassword` — все обязательны.
Ответ `201` — созданный объект (как выше). Дубликат `lmsLogin` → `409`.

### `PUT /api/students/{id}` — обновить студента

Все поля опциональны; `lmsPassword` не передан → пароль не меняется. Ответ `200` —
обновлённый объект. Несуществующий `id` → `404`.

### `DELETE /api/students/{id}` — удалить студента

Ответ `204`.

### `POST /api/mark` — запуск отметки (SSE)

Тело:

```json
{
  "qrUrl": "https://lms.uni-dubna.ru/mod/attendance/attendance.php?sessid=123456&qrpass=abc",
  "studentIds": [1, 5, 12]
}
```

- `qrUrl` — сырая ссылка из QR-кода, обязателен; host должен быть `lms.uni-dubna.ru`,
  иначе `400`;
- `studentIds` не передан или пуст → отмечаются все студенты с `is_selected = true`;
- нечего отмечать → `400`.

Успешный ответ — **`text/event-stream`**. Сервис разбирает студентов по пулу воркеров
параллельно и по мере готовности каждого пишет событие:

```
event: result
data: {"login":"ivanov","status":"success","message":"Отмечено"}

event: result
data: {"login":"petrov","status":"already","message":"Посещаемость уже была отмечена"}

event: result
data: {"login":"sidorov","status":"error","message":"Неверный логин/пароль"}

event: done
data: {"total":3,"success":1,"already":1,"error":1}
```

- порядок `result` произвольный (завершение параллельное);
- `status`: `success` — отметка сохранена; `already` — уже была отмечена (считается
  успехом); `error` — сбой этого студента (поток не прерывается);
- heartbeat: комментарий-строка `: ping` каждые ~15 с, чтобы прокси не рвали
  соединение;
- обрыв соединения клиентом не отменяет начатые отметки; возобновления потока
  в v1 нет — клиент показывает полученное и «нет данных» по остальным.

### `GET /health` — статус

`200` `{"status":"ok","database":"up"}`; недоступная БД → `503` и `database: down`.
Без авторизации.

---

## 3. Механика отметки в Moodle (внутри сервиса)

На каждого студента — **отдельный `http.Client` с собственным cookie jar**
(сессии не смешиваются), таймаут запроса ~15 с. Заголовки имитируют десктопный
браузер: `User-Agent` (Chrome), `Referer`, `Origin: https://lms.uni-dubna.ru`.

### Логин

1. `GET /login/index.php` → из HTML извлекается скрытое поле `logintoken`.
2. `POST /login/index.php`, форма `application/x-www-form-urlencoded`:
   `username`, `password`, `rememberusername=0`, `logintoken`.
3. Успех = в ответе нет формы логина (`logintoken` / `loginerrormessage`).
   Cookies `MoodleSession` остаются в cookie jar этого студента.

### Отметка

1. `GET /mod/attendance/attendance.php?sessid={sessid}[&qrpass={qrpass}]`:
   - в ответе `<title>Вход на сайт` → сессия слетела → один повторный логин и
     повтор страницы;
   - текст содержит «уже была отмечена» / «already been marked» → статус `already`;
   - из HTML извлекается `sesskey`;
   - из радиокнопок `input[type="radio"][name="status"]` выбирается та, чья подпись
     содержит одно из слов: `present`, `присутств`, `был`, `была`, `были`, `явился`,
     `явилась`, `уважительн`; совпадений нет — берётся первая. **Хардкод `status=1`
     из старого `MoodleClient` не переносится** — ID статуса не обязан быть `1`.
2. `POST /mod/attendance/attendance.php?sessid={sessid}&mark=1`, форма:
   `sessid`, `sesskey`, `status={выбранный}`, `submitbutton=Save`; при наличии
   пароля QR — `studentpassword={qrpass}` и `qrpass={qrpass}`.
3. Результат по HTML ответа:
   - `saved` / `сохранено` / `успешно` → `success`;
   - `already` / `уже отмечен` → `already`;
   - иначе → `error`.

> Константа `view.php?id=122864&success=true` из WebView-потока **не переносится** —
> она была нужна только для навигации встроенного браузера.

### Параллельность

- пул воркеров, лимит конкурентности из env (по умолчанию **5**);
- десятки одновременных логинов с одного IP заметны администратору LMS — лимит
  ограничивает и нагрузку, и заметность; конфигурация через env-переменные
  (`server/README.md`, появляется вместе с кодом сервиса).

---

## 4. HTTP-точки Moodle (используются сервисом)

| Метод | Путь | Назначение |
|---|---|---|
| GET | `/login/index.php` | Страница входа; содержит скрытое поле `logintoken` |
| POST | `/login/index.php` | Отправка формы входа |
| GET | `/mod/attendance/attendance.php?sessid={id}[&qrpass={pass}]` | Страница отметки; содержит `sesskey` и радиокнопки статуса |
| POST | `/mod/attendance/attendance.php?sessid={id}&mark=1` | Сохранение отметки |

Модель аутентификации — **сессия на cookies** (`MoodleSession`), отдельная на каждого
студента. Базовый URL `https://lms.uni-dubna.ru` захардкожен в сервисе.

---

## 5. Разбор QR-ссылки

Разбор выполняет **сервис** при `POST /api/mark`:

- `sessid` — обязателен, иначе `400`;
- пароль QR — первый непустой из `qrpass`, `password`, `studentpassword`;
- host ссылки обязан быть `lms.uni-dubna.ru`, иначе `400`.

На стороне Android пароль QR больше не извлекается: сканер передаёт сырую ссылку
целиком в `MarkingScreen`, который отправляет её в `/api/mark`. Функция
`parseQrPassword()` удаляется; `QrParser` остаётся в коде без вызова из UI
(см. ADR 0001).

---

## 6. Android-сторона

- Локально хранится **только конфигурация сервиса**: URL и bearer-токен
  (`EncryptedSharedPreferences`, отдельный ключ; схема `accounts_list` больше
  не используется).
- Аккаунты на устройстве не хранятся: список студентов живёт в БД сервиса,
  `AccountsScreen` работает через CRUD-API (пароль в UI после сохранения
  не отображается; пустое поле при редактировании = «не менять»).
- Поток отметки: `ScannerScreen` → сырая QR-ссылка → `MarkingScreen` →
  `POST /api/mark` → построчное чтение SSE-потока (OkHttp) → живой список
  результатов и финальная сводка.

---

## Связанные файлы

- [`openapi.yaml`](openapi.yaml) — формальный контракт API сервиса
- [`adr/0001-go-attendance-service.md`](adr/0001-go-attendance-service.md) — решение о рефакторинге
- [`app-flow.html`](app-flow.html) — диаграмма потока
- [`README.md`](README.md) — индекс документации
- [`../README.md`](../README.md) — пользовательский обзор проекта
