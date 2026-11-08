package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusBadRequest, "что-то не так")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("код ответа = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json; charset=utf-8", got)
	}
	m := decodeJSON(t, rec)
	if m["message"] != "что-то не так" {
		t.Errorf("message = %v, want %q", m["message"], "что-то не так")
	}
}

func TestApp_Auth(t *testing.T) {
	// Внутренний обработчик помечает себя статусом 418: если middleware
	// пустил запрос дальше — увидим именно его.
	a := newTestApp(&fakeStore{})
	handler := a.auth(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{name: "нет заголовка", header: "", want: http.StatusUnauthorized},
		{name: "не Bearer", header: "Basic dGVzdA==", want: http.StatusUnauthorized},
		{name: "неверный токен", header: "Bearer wrong", want: http.StatusUnauthorized},
		{name: "пустой токен", header: "Bearer ", want: http.StatusUnauthorized},
		{name: "токен другой длины", header: "Bearer test-tokens", want: http.StatusUnauthorized},
		{name: "верный токен", header: "Bearer test-token", want: http.StatusTeapot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/students", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			handler(rec, req)
			if rec.Code != tt.want {
				t.Errorf("auth с заголовком %q: код %d, want %d", tt.header, rec.Code, tt.want)
			}
		})
	}
}

func TestApp_HandleHealth(t *testing.T) {
	t.Run("база доступна", func(t *testing.T) {
		a := newTestApp(&fakeStore{})
		rec := do(t, a, http.MethodGet, "/health", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("код = %d, want %d (тело: %s)", rec.Code, http.StatusOK, rec.Body.String())
		}
		m := decodeJSON(t, rec)
		if m["status"] != "ok" || m["database"] != "up" {
			t.Errorf("тело = %v, want status=ok, database=up", m)
		}
	})
	t.Run("база недоступна — 503 degraded", func(t *testing.T) {
		a := newTestApp(&fakeStore{pingErr: errors.New("connection refused")})
		rec := do(t, a, http.MethodGet, "/health", "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("код = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
		m := decodeJSON(t, rec)
		if m["status"] != "degraded" || m["database"] != "down" {
			t.Errorf("тело = %v, want status=degraded, database=down", m)
		}
	})
}

func TestApp_HandleListStudents(t *testing.T) {
	t.Run("список без паролей", func(t *testing.T) {
		st := &fakeStore{list: []Student{{ID: 1, LmsLogin: "ivanov"}}}
		rec := do(t, newTestApp(st), http.MethodGet, "/api/students", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("код = %d, want %d (тело: %s)", rec.Code, http.StatusOK, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.HasPrefix(strings.TrimSpace(body), "[") {
			t.Errorf("ожидался JSON-массив, получено: %s", body)
		}
		if strings.Contains(strings.ToLower(body), "password") {
			t.Errorf("в списке студентов утечка пароля: %s", body)
		}
	})
	t.Run("ошибка БД — 500", func(t *testing.T) {
		st := &fakeStore{listErr: errors.New("boom")}
		rec := do(t, newTestApp(st), http.MethodGet, "/api/students", "")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("код = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
		if m := decodeJSON(t, rec); !strings.HasPrefix(m["message"].(string), "ошибка БД") {
			t.Errorf("message = %v, want префикс «ошибка БД»", m["message"])
		}
	})
}

func TestApp_HandleCreateStudent(t *testing.T) {
	const validBody = `{"firstName":"Иван","lastName":"Иванов","studyGroup":"101-А",` +
		`"lmsLogin":"ivanov","lmsPassword":"secret"}`

	tests := []struct {
		name     string
		body     string
		store    *fakeStore
		wantCode int
	}{
		{
			name:     "успешное создание — 201",
			body:     validBody,
			store:    &fakeStore{createOut: Student{ID: 1, LmsLogin: "ivanov"}},
			wantCode: http.StatusCreated,
		},
		{
			name:     "сломанный JSON — 400",
			body:     `{"firstName":`,
			store:    &fakeStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "пропущено обязательное поле — 400",
			body:     `{"firstName":"Иван","lastName":"Иванов","lmsLogin":"ivanov","lmsPassword":"x"}`,
			store:    &fakeStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name: "дубликат lmsLogin — 409",
			body: validBody,
			store: &fakeStore{
				createOut: Student{ID: 1},
				createErr: errDuplicateLogin,
			},
			wantCode: http.StatusConflict,
		},
		{
			name: "другая ошибка БД — 500",
			body: validBody,
			store: &fakeStore{
				createOut: Student{},
				createErr: errors.New("boom"),
			},
			wantCode: http.StatusInternalServerError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, newTestApp(tt.store), http.MethodPost, "/api/students", tt.body)
			if rec.Code != tt.wantCode {
				t.Fatalf("код = %d, want %d (тело: %s)", rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}

	t.Run("созданному студенту возвращается тело из store", func(t *testing.T) {
		st := &fakeStore{createOut: Student{ID: 9, LmsLogin: "petrov"}}
		rec := do(t, newTestApp(st), http.MethodPost, "/api/students", validBody)
		m := decodeJSON(t, rec)
		if m["lmsLogin"] != "petrov" || m["id"] != float64(9) {
			t.Errorf("тело ответа = %v, want id=9, lmsLogin=petrov", m)
		}
		if st.createIn.LmsPassword != "secret" || st.createIn.FirstName != "Иван" {
			t.Errorf("в store передано %v+secret, want поля из тела запроса", st.createIn)
		}
	})
}

func TestParseStudentId(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		want    int64
		wantErr bool
	}{
		{name: "числовой id", id: "42", want: 42},
		{name: "не число", id: "abc", wantErr: true},
		{name: "переполнение int64", id: "99999999999999999999", wantErr: true},
		{name: "отрицательный — числово валиден, но не существует", id: "-1", want: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/students/{id}", nil)
			req.SetPathValue("id", tt.id)
			got, err := parseStudentId(req)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseStudentId(%q) = %d, ожидается ошибка", tt.id, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseStudentId(%q) неожиданная ошибка: %v", tt.id, err)
			}
			if got != tt.want {
				t.Errorf("parseStudentId(%q) = %d, want %d", tt.id, got, tt.want)
			}
		})
	}
}

func TestApp_HandleUpdateStudent(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		body     string
		store    *fakeStore
		wantCode int
	}{
		{
			name:     "некорректный id — 400",
			id:       "abc",
			body:     `{}`,
			store:    &fakeStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "сломанный JSON — 400",
			id:       "1",
			body:     `{`,
			store:    &fakeStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "пустое поле — 400",
			id:       "1",
			body:     `{"firstName":""}`,
			store:    &fakeStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "поле длиннее лимита — 400",
			id:       "1",
			body:     `{"studyGroup":"` + strings.Repeat("г", 51) + `"}`,
			store:    &fakeStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name: "студент не найден — 404",
			id:   "1",
			body: `{"firstName":"Пётр"}`,
			store: &fakeStore{
				updateOut: Student{},
				updateErr: errNotFound,
			},
			wantCode: http.StatusNotFound,
		},
		{
			name: "дубликат логина — 409",
			id:   "1",
			body: `{"lmsLogin":"ivanov"}`,
			store: &fakeStore{
				updateOut: Student{},
				updateErr: errDuplicateLogin,
			},
			wantCode: http.StatusConflict,
		},
		{
			name: "другая ошибка БД — 500",
			id:   "1",
			body: `{}`,
			store: &fakeStore{
				updateOut: Student{},
				updateErr: errors.New("boom"),
			},
			wantCode: http.StatusInternalServerError,
		},
		{
			name:     "пустой объект — 200 (текущее состояние)",
			id:       "1",
			body:     `{}`,
			store:    &fakeStore{updateOut: Student{ID: 1, LmsLogin: "ivanov"}},
			wantCode: http.StatusOK,
		},
		{
			name:     "частичное обновление — 200",
			id:       "1",
			body:     `{"studyGroup":"202-Б","isSelected":false}`,
			store:    &fakeStore{updateOut: Student{ID: 1, StudyGroup: "202-Б"}},
			wantCode: http.StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/students/{id}", strings.NewReader(tt.body))
			req.SetPathValue("id", tt.id)
			req.Header.Set("Authorization", "Bearer test-token")
			rec := httptest.NewRecorder()
			newTestApp(tt.store).handleUpdateStudent(rec, req)
			if rec.Code != tt.wantCode {
				t.Fatalf("код = %d, want %d (тело: %s)", rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}

	t.Run("переданы только указанные поля", func(t *testing.T) {
		st := &fakeStore{updateOut: Student{ID: 3}}
		req := httptest.NewRequest(http.MethodPut, "/api/students/{id}",
			strings.NewReader(`{"studyGroup":"202-Б"}`))
		req.SetPathValue("id", "3")
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		newTestApp(st).handleUpdateStudent(rec, req)

		if st.updateID != 3 {
			t.Errorf("в store передан id %d, want 3", st.updateID)
		}
		in := st.updateIn
		if in.FirstName != nil || in.LastName != nil || in.LmsLogin != nil || in.LmsPassword != nil {
			t.Errorf("непереданные поля не должны попасть в store: %+v", in)
		}
		if in.StudyGroup == nil || *in.StudyGroup != "202-Б" {
			t.Errorf("studyGroup = %v, want указатель на %q", in.StudyGroup, "202-Б")
		}
	})
}

func TestRuneCount(t *testing.T) {
	if got := runeCount("привет"); got != 6 {
		t.Errorf("runeCount(\"привет\") = %d, want 6 (символы, не байты)", got)
	}
	if got := runeCount(""); got != 0 {
		t.Errorf("runeCount(\"\") = %d, want 0", got)
	}
}

func TestApp_HandleDeleteStudent(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		store    *fakeStore
		wantCode int
	}{
		{
			name:     "некорректный id — 400",
			id:       "abc",
			store:    &fakeStore{},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "успешное удаление — 204",
			id:       "5",
			store:    &fakeStore{deleted: true},
			wantCode: http.StatusNoContent,
		},
		{
			name:     "студента нет — 404",
			id:       "5",
			store:    &fakeStore{deleted: false},
			wantCode: http.StatusNotFound,
		},
		{
			name:     "ошибка БД — 500",
			id:       "5",
			store:    &fakeStore{deleteErr: errors.New("boom")},
			wantCode: http.StatusInternalServerError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodDelete, "/api/students/{id}", nil)
			req.SetPathValue("id", tt.id)
			req.Header.Set("Authorization", "Bearer test-token")
			rec := httptest.NewRecorder()
			newTestApp(tt.store).handleDeleteStudent(rec, req)
			if rec.Code != tt.wantCode {
				t.Fatalf("код = %d, want %d (тело: %s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantCode == http.StatusNoContent && rec.Body.Len() != 0 {
				t.Errorf("204 не должен содержать тела, получено: %s", rec.Body.String())
			}
		})
	}
}
