package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// ---- HTML-заготовки страниц Moodle ----

// loginPageHTML — страница входа со скрытым полем logintoken.
func loginPageHTML(token string) string {
	return `<html><form action="/login/index.php" method="post">` +
		`<input type="hidden" name="logintoken" value="` + token + `">` +
		`<input name="username"><input name="password"></form></html>`
}

// attendancePageHTML — страница отметки: sesskey в ссылке выхода и радиокнопки
// статусов, как в настоящем Moodle (ID статусов произвольные, «присутствовал» не первый).
func attendancePageHTML() string {
	return `<html><body>` +
		`<a href="/login/logout.php?sesskey=AbCd123">Выйти</a>` +
		`<div class="statusdiv">` +
		`<label><input type="radio" name="status" value="742"> Отсутствовал</label>` +
		`<label><input type="radio" name="status" value="741"> Присутствовал</label>` +
		`</div></body></html>`
}

// ---- Программируемая заглушка Moodle ----

// moodleScript — заглушка Moodle: запоминает присланные формы и возвращает
// настроенные страницы. Различает студентов по cookie MoodleSession,
// который выставляется при «успешном» логине со значением логина.
type moodleScript struct {
	mu sync.Mutex

	password string // пароль, который принимается; пустой — любой

	loginBody             string // ответ GET /login; пусто — loginPageHTML("TOKEN123")
	failLoginPost         bool   // оборвать соединение на POST логина
	failSecondLoginPost   bool   // отклонить второй POST логина (сессия слетела)
	loginBodyWithoutToken bool   // страница входа без logintoken

	attendanceBody          string // ответ GET страницы отметки; пусто — attendancePageHTML
	attendanceBySession     map[string]string
	relogin                 bool   // первый GET страницы отметки возвращает страницу входа
	failAttendanceGET       bool   // оборвать соединение на GET страницы отметки
	failSecondAttendanceGET bool   // оборвать повторный GET после релогина
	markBody                string // ответ POST отметки; пусто — «Сохранено успешно»
	markBodyBySession       map[string]string
	failMarkPost            bool // оборвать соединение на POST отметки
	delayPerRequest         time.Duration

	// журнал обращений (читать только через методы-снимки ниже)
	loginGets      int
	loginPosts     []url.Values
	attendanceGets int
	markGetQuery   url.Values
	markPost       url.Values
}

// startMoodle поднимает заглушку Moodle как httptest-сервер.
func startMoodle(t *testing.T, script *moodleScript) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(script.handler())
	t.Cleanup(srv.Close)
	return srv
}

func (m *moodleScript) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.delay()
		switch {
		case r.URL.Path == "/login/index.php" && r.Method == http.MethodGet:
			m.mu.Lock()
			m.loginGets++
			body, noToken := m.loginBody, m.loginBodyWithoutToken
			m.mu.Unlock()
			switch {
			case noToken:
				body = `<html><form><input name="username"></form></html>`
			case body == "":
				body = loginPageHTML("TOKEN123")
			}
			writeBody(w, body)

		case r.URL.Path == "/login/index.php" && r.Method == http.MethodPost:
			_ = r.ParseForm()
			m.mu.Lock()
			m.loginPosts = append(m.loginPosts, r.PostForm)
			posts := len(m.loginPosts)
			password := m.password
			fail := m.failLoginPost || (m.failSecondLoginPost && posts >= 2)
			m.mu.Unlock()
			switch {
			case fail:
				panic(http.ErrAbortHandler) // имитация обрыва соединения
			case password != "" && r.PostForm.Get("password") != password:
				writeBody(w, `<html><span>loginerrormessage</span></html>`)
			default:
				http.SetCookie(w, &http.Cookie{
					Name:  "MoodleSession",
					Value: r.PostForm.Get("username"),
					Path:  "/", // как настоящий Moodle: сессия живёт на всём сайте
				})
				writeBody(w, `<html><body>Личный кабинет</body></html>`)
			}

		case r.URL.Path == "/mod/attendance/attendance.php" && r.Method == http.MethodGet:
			m.mu.Lock()
			m.attendanceGets++
			m.markGetQuery = r.URL.Query()
			relogin := m.relogin
			m.relogin = false
			body, bySession := m.attendanceBody, m.attendanceBySession
			fail := m.failAttendanceGET || (m.failSecondAttendanceGET && m.attendanceGets >= 2)
			m.mu.Unlock()
			switch {
			case fail:
				panic(http.ErrAbortHandler)
			case relogin:
				writeBody(w, `<html><head><title>Вход на сайт</title></head></html>`)
				return
			}
			if b, ok := bySession[sessionLogin(r)]; ok {
				body = b
			}
			if body == "" {
				body = attendancePageHTML()
			}
			writeBody(w, body)

		case r.URL.Path == "/mod/attendance/attendance.php" && r.Method == http.MethodPost:
			_ = r.ParseForm()
			m.mu.Lock()
			m.markPost = r.PostForm
			body, bySession := m.markBody, m.markBodyBySession
			fail := m.failMarkPost
			m.mu.Unlock()
			switch {
			case fail:
				panic(http.ErrAbortHandler)
			default:
				if b, ok := bySession[sessionLogin(r)]; ok {
					body = b
				}
				if body == "" {
					body = `Сохранено успешно` // содержит ключевое слово «сохранено»
				}
				writeBody(w, body)
			}

		default:
			http.NotFound(w, r)
		}
	}
}

func (m *moodleScript) delay() {
	m.mu.Lock()
	d := m.delayPerRequest
	m.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
}

// sessionLogin — «логин» студента из cookie сессии (пусто, если сессии нет).
func sessionLogin(r *http.Request) string {
	if c, err := r.Cookie("MoodleSession"); err == nil {
		return c.Value
	}
	return ""
}

// writeBody — запись тела ответа заглушки; ошибка записи в тесте не важна
// (требование errcheck: возвращаемое значение должно использоваться).
func writeBody(w http.ResponseWriter, body string) {
	_, _ = fmt.Fprint(w, body)
}

// Снимки журнала обращений: доступ синхронизируется мьютексом заглушки.

func (m *moodleScript) loginPostCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.loginPosts)
}

func (m *moodleScript) lastLoginPost() url.Values {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.loginPosts) == 0 {
		return nil
	}
	return m.loginPosts[len(m.loginPosts)-1]
}

func (m *moodleScript) attendanceGetCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attendanceGets
}

func (m *moodleScript) markGetQuerySnapshot() url.Values {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.markGetQuery
}

func (m *moodleScript) markPostSnapshot() url.Values {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.markPost
}

// ---- Тесты lms.go (в порядке функций исходника) ----

func TestNewLMSClient(t *testing.T) {
	c := newLMSClient("https://lms.example.com/")
	if c.baseURL != "https://lms.example.com" {
		t.Errorf("baseURL = %q, want без хвостового слэша", c.baseURL)
	}
	if c.http.Timeout != lmsRequestTimeout {
		t.Errorf("Timeout = %v, want %v", c.http.Timeout, lmsRequestTimeout)
	}
	if c.http.Jar == nil {
		t.Error("у клиента должен быть собственный cookie jar")
	}
}

func TestFindLogintoken(t *testing.T) {
	tests := []struct {
		name string
		page string
		want string
	}{
		{
			name: "name перед value",
			page: `<input type="hidden" name="logintoken" value="abc123">`,
			want: "abc123",
		},
		{
			name: "одинарные кавычки",
			page: `<input name='logintoken' value='tok'>`,
			want: "tok",
		},
		{
			name: "value перед name (обратный порядок атрибутов)",
			page: `<input value="tok" class="x" name="logintoken">`,
			want: "tok",
		},
		{
			name: "похожее имя не совпадает",
			page: `<input name="logintoken2" value="x">`,
			want: "",
		},
		{
			name: "токена нет",
			page: `<html><form><input name="username"></form></html>`,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findLogintoken(tt.page); got != tt.want {
				t.Errorf("findLogintoken() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLmsClient_Get(t *testing.T) {
	t.Run("браузерный User-Agent и тело ответа", func(t *testing.T) {
		t.Parallel()
		captured := make(chan *http.Request, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			captured <- r
			writeBody(w, "тело страницы")
		}))
		defer srv.Close()

		c := newLMSClient(srv.URL)
		body, err := c.get(context.Background(), srv.URL+"/some/page")
		if err != nil {
			t.Fatalf("get() неожиданная ошибка: %v", err)
		}
		if body != "тело страницы" {
			t.Errorf("get() = %q, want %q", body, "тело страницы")
		}
		req := <-captured
		if ua := req.Header.Get("User-Agent"); ua != browserUA {
			t.Errorf("User-Agent = %q, want браузерный Chrome", ua)
		}
	})
	t.Run("сервер недоступен — ошибка", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Close() // сразу закрываем

		c := newLMSClient(srv.URL)
		if _, err := c.get(context.Background(), srv.URL+"/x"); err == nil {
			t.Error("get() на закрытом сервере должен вернуть ошибку")
		}
	})
}

func TestLmsClient_PostForm(t *testing.T) {
	t.Parallel()
	// Захватываем снимок запроса внутри хендлера: после его завершения Body уже закрыт.
	type capturedReq struct {
		header http.Header
		form   url.Values
	}
	captured := make(chan capturedReq, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		captured <- capturedReq{header: r.Header.Clone(), form: r.PostForm.Clone()}
		writeBody(w, "принято")
	}))
	defer srv.Close()

	c := newLMSClient(srv.URL)
	form := url.Values{"sesskey": {"AbCd123"}, "status": {"741"}}
	body, err := c.postForm(context.Background(), srv.URL+"/mod/attendance/attendance.php",
		form, "https://lms.example/referer")
	if err != nil {
		t.Fatalf("postForm() неожиданная ошибка: %v", err)
	}
	if body != "принято" {
		t.Errorf("postForm() = %q, want %q", body, "принято")
	}

	req := <-captured
	if ct := req.header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", ct)
	}
	if ref := req.header.Get("Referer"); ref != "https://lms.example/referer" {
		t.Errorf("Referer = %q, want переданный referer", ref)
	}
	if origin := req.header.Get("Origin"); origin != srv.URL {
		t.Errorf("Origin = %q, want %s", origin, srv.URL)
	}
	if ua := req.header.Get("User-Agent"); ua != browserUA {
		t.Errorf("User-Agent = %q, want браузерный Chrome", ua)
	}
	if got := req.form.Encode(); got != form.Encode() {
		t.Errorf("на сервер ушла форма %q, want %q", got, form.Encode())
	}
}

func TestReadBody(t *testing.T) {
	t.Run("маленькое тело читается целиком", func(t *testing.T) {
		t.Parallel()
		resp := &http.Response{Body: io.NopCloser(strings.NewReader("привет"))}
		got, err := readBody(resp)
		if err != nil || got != "привет" {
			t.Errorf("readBody() = %q, %v; want %q, nil", got, err, "привет")
		}
	})
	t.Run("тело больше лимита обрезается", func(t *testing.T) {
		t.Parallel()
		const maxBodySize = 5 << 20
		big := strings.Repeat("a", maxBodySize+16)
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(big))}
		got, err := readBody(resp)
		if err != nil {
			t.Fatalf("readBody() неожиданная ошибка: %v", err)
		}
		if len(got) != maxBodySize {
			t.Errorf("len(readBody()) = %d, want %d", len(got), maxBodySize)
		}
	})
	t.Run("ошибка чтения возвращается", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("обрыв соединения")
		resp := &http.Response{Body: io.NopCloser(failingReader{err: wantErr})}
		if _, err := readBody(resp); !errors.Is(err, wantErr) {
			t.Errorf("readBody() ошибка = %v, want %v", err, wantErr)
		}
	})
}

// failingReader — читатель, который всегда ошибается.
type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

func TestLmsClient_InvalidBaseURL(t *testing.T) {
	t.Parallel()
	// Из baseURL с пробелом не собирается валидный URL — ошибка до любого соединения.
	c := newLMSClient("http://loc alhost")
	if _, err := c.get(context.Background(), c.baseURL+"/page"); err == nil {
		t.Error("get() с невалидным baseURL должен вернуть ошибку")
	}
	if _, err := c.postForm(context.Background(), c.baseURL+"/page",
		url.Values{}, c.baseURL); err == nil {
		t.Error("postForm() с невалидным baseURL должен вернуть ошибку")
	}
}

func TestLmsClient_Login(t *testing.T) {
	t.Run("успешный вход отправляет форму с logintoken", func(t *testing.T) {
		t.Parallel()
		script := &moodleScript{}
		srv := startMoodle(t, script)
		c := newLMSClient(srv.URL)

		if err := c.login(context.Background(), "ivanov", "secret"); err != nil {
			t.Fatalf("login() неожиданная ошибка: %v", err)
		}
		form := script.lastLoginPost()
		if form == nil {
			t.Fatal("форма логина не дошла до заглушки")
		}
		for _, c := range []struct{ key, want string }{
			{"username", "ivanov"},
			{"password", "secret"},
			{"rememberusername", "0"},
			{"logintoken", "TOKEN123"},
		} {
			if got := form.Get(c.key); got != c.want {
				t.Errorf("форма логина: %s = %q, want %q", c.key, got, c.want)
			}
		}
	})
	t.Run("неверный пароль", func(t *testing.T) {
		t.Parallel()
		script := &moodleScript{password: "правильный"}
		srv := startMoodle(t, script)
		c := newLMSClient(srv.URL)

		err := c.login(context.Background(), "ivanov", "неправильный")
		if err == nil || !strings.Contains(err.Error(), "неверный логин или пароль") {
			t.Errorf("login() = %v, want «неверный логин или пароль»", err)
		}
	})
	t.Run("на странице нет logintoken", func(t *testing.T) {
		t.Parallel()
		script := &moodleScript{loginBodyWithoutToken: true}
		srv := startMoodle(t, script)
		c := newLMSClient(srv.URL)

		err := c.login(context.Background(), "ivanov", "secret")
		if err == nil || !strings.Contains(err.Error(), "не найден logintoken") {
			t.Errorf("login() = %v, want «не найден logintoken»", err)
		}
	})
	t.Run("страница входа недоступна", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Close()

		c := newLMSClient(srv.URL)
		err := c.login(context.Background(), "ivanov", "secret")
		if err == nil || !strings.Contains(err.Error(), "не удалось открыть страницу входа") {
			t.Errorf("login() = %v, want «не удалось открыть страницу входа»", err)
		}
	})
	t.Run("отправка формы оборвалась", func(t *testing.T) {
		t.Parallel()
		script := &moodleScript{failLoginPost: true}
		srv := startMoodle(t, script)
		c := newLMSClient(srv.URL)

		err := c.login(context.Background(), "ivanov", "secret")
		if err == nil || !strings.Contains(err.Error(), "ошибка отправки формы входа") {
			t.Errorf("login() = %v, want «ошибка отправки формы входа»", err)
		}
	})
}

func TestPickStatusValue(t *testing.T) {
	tests := []struct {
		name string
		page string
		want string
	}{
		{
			name: "«присутствовал» не первый — выбирается по подписи",
			page: `<label><input type="radio" name="status" value="742"> Отсутствовал</label>` +
				`<label><input type="radio" name="status" value="741"> Присутствовал</label>`,
			want: "741",
		},
		{
			name: "английское Present",
			page: `<label><input type="radio" name="status" value="2"> Present</label>` +
				`<label><input type="radio" name="status" value="1"> Late</label>`,
			want: "2",
		},
		{
			name: "подпись «Был на паре»",
			page: `<label><input type="radio" name="status" value="5"> Был на паре</label>`,
			want: "5",
		},
		{
			name: "подпись «Явился»",
			page: `<label><input type="radio" name="status" value="9"> Явился</label>`,
			want: "9",
		},
		{
			name: "подпись «По уважительной причине»",
			page: `<label><input type="radio" name="status" value="4"> По уважительной причине</label>`,
			want: "4",
		},
		{
			name: "подпись в верхнем регистре",
			page: `<label><input type="radio" name="status" value="3"> ПРИСУТСТВОВАЛ</label>`,
			want: "3",
		},
		{
			name: "нет совпадений — берётся первая кнопка",
			page: `<label><input type="radio" name="status" value="11"> Unknown</label>` +
				`<label><input type="radio" name="status" value="12"> Also unknown</label>`,
			want: "11",
		},
		{
			name: "радиокнопок нет",
			page: `<html><body>занятие закрыто</body></html>`,
			want: "",
		},
		{
			name: "чужие input игнорируются",
			page: `<input type="radio" name="other" value="99">` +
				`<input type="checkbox" name="status" value="98">` +
				`<label><input type="radio" name="status" value="7"> Опоздал</label>`,
			want: "7",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pickStatusValue(tt.page); got != tt.want {
				t.Errorf("pickStatusValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNodeText(t *testing.T) {
	doc, err := html.Parse(strings.NewReader(`<div>A<b>B</b>C</div>`))
	if err != nil {
		t.Fatalf("html.Parse: %v", err)
	}
	var div *html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "div" {
			div = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if div == nil {
		t.Fatal("div не найден")
	}
	if got := nodeText(div); got != "ABC" {
		t.Errorf("nodeText() = %q, want %q", got, "ABC")
	}
	if got := nodeText(nil); got != "" {
		t.Errorf("nodeText(nil) = %q, want пустая строка", got)
	}
}

func TestLmsClient_MarkAttendance(t *testing.T) {
	const (
		login    = "ivanov"
		password = "secret"
		sessid   = "77"
	)

	tests := []struct {
		name            string
		script          *moodleScript
		qrpass          string
		wantStatus      string
		wantMsgContains string
		check           func(t *testing.T, s *moodleScript)
	}{
		{
			name:            "успешная отметка: sessid, sesskey и статус по подписи",
			script:          &moodleScript{},
			wantStatus:      MarkStatusSuccess,
			wantMsgContains: "Отмечено",
			check: func(t *testing.T, s *moodleScript) {
				form := s.markPostSnapshot()
				if form == nil {
					t.Fatal("форма отметки не дошла до заглушки")
				}
				for _, c := range []struct{ key, want string }{
					{"sessid", sessid},
					{"sesskey", "AbCd123"},
					{"status", "741"}, // «Присутствовал», а не первая кнопка и не хардкод 1
					{"submitbutton", "Save"},
				} {
					if got := form.Get(c.key); got != c.want {
						t.Errorf("форма отметки: %s = %q, want %q", c.key, got, c.want)
					}
				}
				if got := form.Get("qrpass"); got != "" {
					t.Errorf("без qrpass поле qrpass не отправляется, получено %q", got)
				}
			},
		},
		{
			name:            "с qrpass: пароль уходит в форму и на страницу отметки",
			script:          &moodleScript{},
			qrpass:          "pw123",
			wantStatus:      MarkStatusSuccess,
			wantMsgContains: "Отмечено",
			check: func(t *testing.T, s *moodleScript) {
				if got := s.markGetQuerySnapshot().Get("qrpass"); got != "pw123" {
					t.Errorf("qrpass на странице отметки = %q, want pw123", got)
				}
				form := s.markPostSnapshot()
				if got := form.Get("studentpassword"); got != "pw123" {
					t.Errorf("studentpassword = %q, want pw123", got)
				}
				if got := form.Get("qrpass"); got != "pw123" {
					t.Errorf("qrpass = %q, want pw123", got)
				}
			},
		},
		{
			name: "уже была отмечена — определяется по странице",
			script: &moodleScript{
				attendanceBody: `<html><body>Эта посещаемость уже была отмечена</body></html>`,
			},
			wantStatus:      MarkStatusAlready,
			wantMsgContains: "уже была отмечена",
			check: func(t *testing.T, s *moodleScript) {
				if s.markPostSnapshot() != nil {
					t.Error("при «уже отмечена» форма отметки отправляться не должна")
				}
			},
		},
		{
			name: "уже была отмечена — определяется по ответу POST",
			script: &moodleScript{
				markBody: `<html>Already marked by someone else</html>`,
			},
			wantStatus:      MarkStatusAlready,
			wantMsgContains: "уже была отмечена",
		},
		{
			name: "ответ без подтверждения — ошибка",
			script: &moodleScript{
				markBody: `<html>что-то пошло не так</html>`,
			},
			wantStatus:      MarkStatusError,
			wantMsgContains: "ответ получен, но отметка не подтвердилась",
		},
		{
			name:            "неверный пароль — ошибка логина",
			script:          &moodleScript{password: "другой"},
			wantStatus:      MarkStatusError,
			wantMsgContains: "неверный логин или пароль",
		},
		{
			name: "нет sesskey",
			script: &moodleScript{
				attendanceBody: `<html><body><label><input type="radio" name="status" value="1"> Был</label></body></html>`,
			},
			wantStatus:      MarkStatusError,
			wantMsgContains: "не найден sesskey",
		},
		{
			name: "нет радиокнопок статуса",
			script: &moodleScript{
				attendanceBody: `<html><body><a href="/login/logout.php?sesskey=AbCd123">Выйти</a></body></html>`,
			},
			wantStatus:      MarkStatusError,
			wantMsgContains: "нет радиокнопок",
		},
		{
			name:            "страница отметки недоступна",
			script:          &moodleScript{failAttendanceGET: true},
			wantStatus:      MarkStatusError,
			wantMsgContains: "не удалось открыть страницу отметки",
		},
		{
			name:            "отправка формы отметки оборвалась",
			script:          &moodleScript{failMarkPost: true},
			wantStatus:      MarkStatusError,
			wantMsgContains: "ошибка отправки формы отметки",
		},
		{
			name:            "сессия слетела — повторный логин ведёт к успеху",
			script:          &moodleScript{relogin: true},
			wantStatus:      MarkStatusSuccess,
			wantMsgContains: "Отмечено",
			check: func(t *testing.T, s *moodleScript) {
				if n := s.loginPostCount(); n != 2 {
					t.Errorf("логинов = %d, want 2 (первый + повторный после слёта сессии)", n)
				}
				if n := s.attendanceGetCount(); n != 2 {
					t.Errorf("GET страниц отметки = %d, want 2", n)
				}
			},
		},
		{
			name:            "сессия слетела и повторный вход не удался",
			script:          &moodleScript{relogin: true, failSecondLoginPost: true},
			wantStatus:      MarkStatusError,
			wantMsgContains: "повторный вход не удался",
		},
		{
			name:            "сессия слетела и повторная страница не открылась",
			script:          &moodleScript{relogin: true, failSecondAttendanceGET: true},
			wantStatus:      MarkStatusError,
			wantMsgContains: "не удалось открыть страницу отметки",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := startMoodle(t, tt.script)
			c := newLMSClient(srv.URL)

			res := c.MarkAttendance(context.Background(), login, password, sessid, tt.qrpass)
			if res.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q (message: %q)", res.Status, tt.wantStatus, res.Message)
			}
			if !strings.Contains(res.Message, tt.wantMsgContains) {
				t.Errorf("message = %q, want содержит %q", res.Message, tt.wantMsgContains)
			}
			if res.Login != login {
				t.Errorf("login = %q, want %q", res.Login, login)
			}
			if tt.check != nil {
				tt.check(t, tt.script)
			}
		})
	}

	t.Run("Moodle недоступен целиком", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Close()
		c := newLMSClient(srv.URL)

		res := c.MarkAttendance(context.Background(), login, password, sessid, "")
		if res.Status != MarkStatusError || !strings.Contains(res.Message, "не удалось открыть страницу входа") {
			t.Errorf("MarkAttendance() = %+v, want ошибка открытия страницы входа", res)
		}
	})
}
