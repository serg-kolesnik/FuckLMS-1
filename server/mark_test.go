package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

const testLMSHost = "lms.uni-dubna.ru"

func TestParseQRUrl(t *testing.T) {
	const base = "https://" + testLMSHost

	tests := []struct {
		name     string
		raw      string
		lmsBase  string
		wantSess string
		wantPass string
		wantErr  string // подстрока ожидаемой ошибки; пусто — ошибки быть не должно
	}{
		{
			name:     "sessid и qrpass",
			raw:      base + "/mod/attendance/attendance.php?sessid=123456&qrpass=abc",
			wantSess: "123456",
			wantPass: "abc",
		},
		{
			name:     "qrpass важнее password",
			raw:      base + "/mod/attendance/attendance.php?sessid=1&qrpass=a&password=b",
			wantSess: "1",
			wantPass: "a",
		},
		{
			name:     "password важнее studentpassword",
			raw:      base + "/mod/attendance/attendance.php?sessid=1&password=b&studentpassword=c",
			wantSess: "1",
			wantPass: "b",
		},
		{
			name:     "fallback на studentpassword",
			raw:      base + "/mod/attendance/attendance.php?sessid=1&studentpassword=c",
			wantSess: "1",
			wantPass: "c",
		},
		{
			name:     "пароля нет — пустая строка",
			raw:      base + "/mod/attendance/attendance.php?sessid=1",
			wantSess: "1",
			wantPass: "",
		},
		{
			name:    "нет sessid",
			raw:     base + "/mod/attendance/attendance.php?qrpass=abc",
			wantErr: "отсутствует sessid",
		},
		{
			name:    "чужой host",
			raw:     "https://evil.example/mod/attendance/attendance.php?sessid=1",
			wantErr: "чужой host",
		},
		{
			name:    "host с портом не совпадает",
			raw:     base + ":8443/mod/attendance/attendance.php?sessid=1",
			wantErr: "чужой host",
		},
		{
			name:    "непарсируемая ссылка",
			raw:     "://не-ссылка",
			wantErr: "qrUrl не парсится",
		},
		{
			name:    "непарсируемый LMS_BASE_URL",
			raw:     base + "/mod/attendance/attendance.php?sessid=1",
			lmsBase: "://не-ссылка",
			wantErr: "LMS_BASE_URL не парсится",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lmsBase := tt.lmsBase
			if lmsBase == "" {
				lmsBase = base
			}
			sessid, qrpass, err := parseQRUrl(tt.raw, lmsBase)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseQRUrl() = %q, %q, %v; want ошибка с %q", sessid, qrpass, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseQRUrl() неожиданная ошибка: %v", err)
			}
			if sessid != tt.wantSess || qrpass != tt.wantPass {
				t.Errorf("parseQRUrl() = (%q, %q), want (%q, %q)",
					sessid, qrpass, tt.wantSess, tt.wantPass)
			}
		})
	}
}

func TestApp_SelectStudents(t *testing.T) {
	t.Run("явный список id — запрос по id", func(t *testing.T) {
		t.Parallel()
		st := &fakeStore{
			byIDs: []studentRecord{{Student: Student{ID: 1}, LmsPassword: "p"}},
		}
		a := newTestApp(st)

		got, err := a.selectStudents(context.Background(), []int64{1, 5})
		if err != nil {
			t.Fatalf("selectStudents() неожиданная ошибка: %v", err)
		}
		if len(got) != 1 || got[0].ID != 1 {
			t.Errorf("selectStudents() = %+v, want одну запись с id=1", got)
		}
		if len(st.byIDsIn) != 2 || st.byIDsIn[0] != 1 || st.byIDsIn[1] != 5 {
			t.Errorf("в store переданы id %v, want [1 5]", st.byIDsIn)
		}
	})
	t.Run("список пуст — берутся выбранные студенты", func(t *testing.T) {
		t.Parallel()
		st := &fakeStore{
			selected: []studentRecord{{Student: Student{ID: 2}, LmsPassword: "p"}},
		}
		a := newTestApp(st)

		got, err := a.selectStudents(context.Background(), nil)
		if err != nil {
			t.Fatalf("selectStudents() неожиданная ошибка: %v", err)
		}
		if len(got) != 1 || got[0].ID != 2 {
			t.Errorf("selectStudents() = %+v, want одну запись с id=2", got)
		}
		if st.byIDsIn != nil {
			t.Errorf("при пустом списке не должен запрашиваться StudentsByIDs, передано %v", st.byIDsIn)
		}
	})
}

// sseEvent — одно распарсенное SSE-событие.
type sseEvent struct {
	Name string
	Data string
}

// parseSSE разбираивает тело SSE-ответа на события; heartbeat-комментарии
// (строки, начинающиеся с «:») пропускаются.
func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var events []sseEvent
	var cur sseEvent
	flush := func() {
		if cur.Name != "" || cur.Data != "" {
			events = append(events, cur)
			cur = sseEvent{}
		}
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// keep-alive комментарий
		case strings.HasPrefix(line, "event: "):
			cur.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		}
	}
	flush()
	return events
}

// markRequestBody — тело POST /api/mark из QR-ссылки и списка id.
func markRequestBody(qrURL, ids string) string {
	return `{"qrUrl":"` + qrURL + `","studentIds":` + ids + `}`
}

// qrURLFor — QR-ссылка с host'ом тестовой заглушки Moodle.
func qrURLFor(srvURL string) string {
	return srvURL + "/mod/attendance/attendance.php?sessid=77&qrpass=pw"
}

func TestApp_HandleMark(t *testing.T) {
	t.Run("неверное тело запроса — 400", func(t *testing.T) {
		t.Parallel()
		a := newTestApp(&fakeStore{})
		rec := do(t, a, http.MethodPost, "/api/mark", `{qrUrl:`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("код = %d, want 400 (тело: %s)", rec.Code, rec.Body.String())
		}
	})
	t.Run("qrUrl без sessid — 400", func(t *testing.T) {
		t.Parallel()
		a := newTestApp(&fakeStore{})
		rec := do(t, a, http.MethodPost, "/api/mark", `{"qrUrl":"https://lms.uni-dubna.ru/x"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("код = %d, want 400 (тело: %s)", rec.Code, rec.Body.String())
		}
	})
	t.Run("чужой host в qrUrl — 400", func(t *testing.T) {
		t.Parallel()
		a := newTestApp(&fakeStore{})
		rec := do(t, a, http.MethodPost, "/api/mark", `{"qrUrl":"https://evil.example/x?sessid=1"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("код = %d, want 400 (тело: %s)", rec.Code, rec.Body.String())
		}
	})
	t.Run("ошибка БД при выборе студентов — 500", func(t *testing.T) {
		t.Parallel()
		a := newTestApp(&fakeStore{byIDsErr: fmt.Errorf("boom")})
		rec := do(t, a, http.MethodPost, "/api/mark", `{"qrUrl":"https://lms.uni-dubna.ru/x?sessid=1","studentIds":[1]}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("код = %d, want 500 (тело: %s)", rec.Code, rec.Body.String())
		}
	})
	t.Run("нет студентов для отметки — 400", func(t *testing.T) {
		t.Parallel()
		a := newTestApp(&fakeStore{})
		rec := do(t, a, http.MethodPost, "/api/mark", `{"qrUrl":"https://lms.uni-dubna.ru/x?sessid=1"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("код = %d, want 400 (тело: %s)", rec.Code, rec.Body.String())
		}
		if m := decodeJSON(t, rec); !strings.Contains(m["message"].(string), "нет студентов") {
			t.Errorf("message = %v, want «нет студентов…»", m["message"])
		}
	})
	t.Run("соединение без Flusher — 500", func(t *testing.T) {
		t.Parallel()
		a := newTestApp(&fakeStore{
			byIDs: []studentRecord{{Student: Student{ID: 1, LmsLogin: "ivanov"}, LmsPassword: "p"}},
		})

		req := httptest.NewRequest(http.MethodPost, "/api/mark",
			strings.NewReader(markRequestBody(qrURLFor("https://lms.uni-dubna.ru"), "[1]")))
		req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
		rec := httptest.NewRecorder()
		a.handleMark(stripFlusher{rw: rec}, req) // Recorder реализует Flusher — снимаем его

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("код = %d, want 500 (тело: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("SSE-поток: по событию на студента и сводка done", func(t *testing.T) {
		t.Parallel()
		script := &moodleScript{
			password: "good",
			attendanceBySession: map[string]string{
				// petrov приходит на уже отмеченную сессию
				"petrov": `<html>Эта посещаемость уже была отмечена</html>`,
			},
			markBodyBySession: map[string]string{
				"ivanov": `<html>Saved</html>`,
			},
			// sidorov в store с неверным паролем — упадёт на логине
		}
		srv := startMoodle(t, script)
		st := &fakeStore{
			byIDs: []studentRecord{
				{Student: Student{ID: 1, LmsLogin: "ivanov"}, LmsPassword: "good"},
				{Student: Student{ID: 2, LmsLogin: "petrov"}, LmsPassword: "good"},
				{Student: Student{ID: 3, LmsLogin: "sidorov"}, LmsPassword: "bad"},
			},
		}
		a := newTestApp(st)
		a.cfg.LMSBaseURL = srv.URL // Moodle-заглушка вместо реального LMS

		rec := do(t, a, http.MethodPost, "/api/mark", markRequestBody(qrURLFor(srv.URL), "[1,2,3]"))
		if rec.Code != http.StatusOK {
			t.Fatalf("код = %d, want 200 (тело: %s)", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
			t.Errorf("Content-Type = %q, want text/event-stream", ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("Cache-Control = %q, want no-cache", cc)
		}

		events := parseSSE(t, rec.Body.String())
		if len(events) != 4 {
			t.Fatalf("событий = %d, want 4 (3 result + done): %q", len(events), rec.Body.String())
		}

		// Порядок result произвольный — собираем статусы по логинам.
		statuses := map[string]string{}
		for _, ev := range events[:3] {
			if ev.Name != "result" {
				t.Fatalf("событие = %q, want result", ev.Name)
			}
			var res MarkResult
			if err := json.Unmarshal([]byte(ev.Data), &res); err != nil {
				t.Fatalf("data result не JSON: %v (%s)", err, ev.Data)
			}
			statuses[res.Login] = res.Status
		}
		wantStatuses := map[string]string{
			"ivanov":  MarkStatusSuccess,
			"petrov":  MarkStatusAlready,
			"sidorov": MarkStatusError,
		}
		for login, want := range wantStatuses {
			if got := statuses[login]; got != want {
				t.Errorf("статус %s = %q, want %q (все: %v)", login, got, want, statuses)
			}
		}

		last := events[len(events)-1]
		if last.Name != "done" {
			t.Fatalf("последнее событие = %q, want done", last.Name)
		}
		var summary MarkSummary
		if err := json.Unmarshal([]byte(last.Data), &summary); err != nil {
			t.Fatalf("data done не JSON: %v (%s)", err, last.Data)
		}
		wantSummary := MarkSummary{Total: 3, Success: 1, Already: 1, Error: 1}
		if summary != wantSummary {
			t.Errorf("сводка = %+v, want %+v", summary, wantSummary)
		}
	})
	t.Run("без studentIds отмечаются выбранные студенты", func(t *testing.T) {
		t.Parallel()
		script := &moodleScript{}
		srv := startMoodle(t, script)
		st := &fakeStore{
			selected: []studentRecord{
				{Student: Student{ID: 1, LmsLogin: "ivanov"}, LmsPassword: "good"},
			},
		}
		a := newTestApp(st)
		a.cfg.LMSBaseURL = srv.URL

		rec := do(t, a, http.MethodPost, "/api/mark", markRequestBody(qrURLFor(srv.URL), "[]"))
		if rec.Code != http.StatusOK {
			t.Fatalf("код = %d, want 200 (тело: %s)", rec.Code, rec.Body.String())
		}
		events := parseSSE(t, rec.Body.String())
		if len(events) != 2 || events[0].Name != "result" || events[1].Name != "done" {
			t.Fatalf("события = %+v, want result + done", events)
		}
		var res MarkResult
		if err := json.Unmarshal([]byte(events[0].Data), &res); err != nil {
			t.Fatalf("data result не JSON: %v", err)
		}
		if res.Login != "ivanov" || res.Status != MarkStatusSuccess {
			t.Errorf("result = %+v, want ivanov/success", res)
		}
		if st.byIDsIn != nil {
			t.Errorf("без studentIds не должен вызываться StudentsByIDs, передано %v", st.byIDsIn)
		}
	})

	t.Run("обрыв соединения при стриме — поток завершается", func(t *testing.T) {
		t.Parallel()
		script := &moodleScript{}
		srv := startMoodle(t, script)
		a := newTestApp(&fakeStore{
			selected: []studentRecord{
				{Student: Student{ID: 1, LmsLogin: "ivanov"}, LmsPassword: "p"},
			},
		})
		a.cfg.LMSBaseURL = srv.URL

		req := httptest.NewRequest(http.MethodPost, "/api/mark",
			strings.NewReader(markRequestBody(qrURLFor(srv.URL), "[]")))
		req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
		fw := &failingWriter{header: http.Header{}}
		a.handleMark(fw, req) // первый result записывается, запись done падает — handler обязан вернуться

		if fw.writes != 2 {
			t.Errorf("записей = %d, want 2 (result записан, done оборван)", fw.writes)
		}
	})
}

// failingWriter — ResponseWriter с Flush, имитирующий обрыв соединения:
// первая запись события проходит, вторая падает.
type failingWriter struct {
	header http.Header
	writes int
}

func (f *failingWriter) Header() http.Header { return f.header }

func (f *failingWriter) Write(p []byte) (int, error) {
	f.writes++
	if f.writes >= 2 {
		return 0, errors.New("соединение оборвано")
	}
	return len(p), nil
}

func (f *failingWriter) WriteHeader(int) {}

func (f *failingWriter) Flush() {}

// stripFlusher — ResponseWriter без метода Flush: http.Flusher не реализуется,
// имитирует соединение, не поддерживающее SSE.
type stripFlusher struct {
	rw http.ResponseWriter
}

func (s stripFlusher) Header() http.Header         { return s.rw.Header() }
func (s stripFlusher) Write(p []byte) (int, error) { return s.rw.Write(p) }
func (s stripFlusher) WriteHeader(code int)        { s.rw.WriteHeader(code) }

func TestApp_HandleMark_Heartbeat(t *testing.T) {
	// Каждый запрос к Moodle-заглушке спит по 6 секунд (синтетическое время):
	// логин GET + логин POST + страница отметки + POST отметки = 24 с > 15 с
	// интервала heartbeat, при этом каждый запрос укладывается в lmsRequestTimeout.
	// synctest проматывает часы детерминированно: ping обязан прийти раньше result.
	//
	// Заглушка поднята на in-memory сети (httptest.NewTestServer): реальный сокет
	// внутри пузыря synctest время не проматывает. lmsClient создаёт собственный
	// http.Client без Transport — при каждом запросе он читает http.DefaultTransport,
	// поэтому в этом тесте (последовательном: parallel-субтесты других тестов уже
	// завершились) безопасно подменить его транспортом in-memory сервера.
	synctest.Test(t, func(t *testing.T) {
		script := &moodleScript{delayPerRequest: 6 * time.Second}
		srv := httptest.NewTestServer(t, script.handler())

		origTransport := http.DefaultTransport
		http.DefaultTransport = srv.Client().Transport
		defer func() {
			srv.Client().CloseIdleConnections() // goroutine-ы коннектов не должны пережить пузырь
			srv.Close()
			http.DefaultTransport = origTransport
		}()

		a := newTestApp(&fakeStore{
			selected: []studentRecord{
				{Student: Student{ID: 1, LmsLogin: "ivanov"}, LmsPassword: "p"},
			},
		})
		a.cfg.LMSBaseURL = srv.URL

		req := httptest.NewRequest(http.MethodPost, "/api/mark",
			strings.NewReader(markRequestBody(qrURLFor(srv.URL), "[]")))
		req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
		rec := httptest.NewRecorder()
		a.handleMark(rec, req)

		body := rec.Body.String()
		pingAt := strings.Index(body, ": ping")
		resultAt := strings.Index(body, "event: result")
		if pingAt == -1 {
			t.Fatalf("heartbeat «: ping» не пришёл за долгую отметку (тело: %q)", body)
		}
		if resultAt == -1 || pingAt > resultAt {
			t.Errorf("порядок нарушён: ping на %d, result на %d (тело: %q)", pingAt, resultAt, body)
		}
		if n := strings.Count(body, ": ping"); n != 1 {
			t.Errorf("heartbeat-строк = %d, want 1 (тело: %q)", n, body)
		}
		events := parseSSE(t, body)
		if len(events) != 2 || events[1].Name != "done" {
			t.Fatalf("события = %+v, want result + done", events)
		}
	})
}
