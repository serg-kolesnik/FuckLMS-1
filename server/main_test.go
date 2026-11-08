package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeStore — подмена studentStore для тестов обработчиков: запоминает
// переданные аргументы и возвращает настроенные результаты.
type fakeStore struct {
	pingErr error

	list    []Student
	listErr error

	createIn  StudentCreate
	createOut Student
	createErr error

	updateID  int64
	updateIn  StudentUpdate
	updateOut Student
	updateErr error

	deleteID  int64
	deleted   bool
	deleteErr error

	byIDsIn  []int64
	byIDs    []studentRecord
	byIDsErr error

	selected    []studentRecord
	selectedErr error
}

func (f *fakeStore) Ping(context.Context) error { return f.pingErr }

func (f *fakeStore) ListStudents(context.Context) ([]Student, error) {
	return f.list, f.listErr
}

func (f *fakeStore) CreateStudent(_ context.Context, in StudentCreate) (Student, error) {
	f.createIn = in
	return f.createOut, f.createErr
}

func (f *fakeStore) UpdateStudent(_ context.Context, id int64, in StudentUpdate) (Student, error) {
	f.updateID, f.updateIn = id, in
	return f.updateOut, f.updateErr
}

func (f *fakeStore) DeleteStudent(_ context.Context, id int64) (bool, error) {
	f.deleteID = id
	return f.deleted, f.deleteErr
}

func (f *fakeStore) StudentsByIDs(_ context.Context, ids []int64) ([]studentRecord, error) {
	f.byIDsIn = ids
	return f.byIDs, f.byIDsErr
}

func (f *fakeStore) SelectedStudents(context.Context) ([]studentRecord, error) {
	return f.selected, f.selectedErr
}

// newTestApp — приложение с тестовым токеном и заданным хранилищем.
func newTestApp(store studentStore) *app {
	return &app{
		cfg:   Config{Token: "test-token", Concurrency: 2, LMSBaseURL: "https://lms.uni-dubna.ru"},
		store: store,
	}
}

// do — запрос к роутеру приложения с авторизационным токеном теста.
func do(t *testing.T, a *app, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	return rec
}

// decodeJSON — разбор JSON-тела ответа в карту (для проверки контракта).
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("ответ не является JSON-объектом %q: %v", rec.Body.String(), err)
	}
	return m
}

func TestApp_Routes(t *testing.T) {
	a := newTestApp(&fakeStore{})

	tests := []struct {
		name       string
		method     string
		target     string
		authHeader string // пусто — без токена
		want       int
	}{
		{name: "health доступен без токена", method: http.MethodGet, target: "/health", want: http.StatusOK},
		{name: "список студентов без токена — 401", method: http.MethodGet, target: "/api/students", want: http.StatusUnauthorized},
		{name: "создание без токена — 401", method: http.MethodPost, target: "/api/students", want: http.StatusUnauthorized},
		{name: "обновление без токена — 401", method: http.MethodPut, target: "/api/students/1", want: http.StatusUnauthorized},
		{name: "удаление без токена — 401", method: http.MethodDelete, target: "/api/students/1", want: http.StatusUnauthorized},
		{name: "отметка без токена — 401", method: http.MethodPost, target: "/api/mark", want: http.StatusUnauthorized},
		{name: "с токеном список доступен", method: http.MethodGet, target: "/api/students", authHeader: "ok", want: http.StatusOK},
		{name: "с токеном нечисловой id — доходит до обработчика, 400", method: http.MethodPut, target: "/api/students/abc", authHeader: "ok", want: http.StatusBadRequest},
		{name: "неизвестный путь — 404", method: http.MethodGet, target: "/api/unknown", authHeader: "ok", want: http.StatusNotFound},
		{name: "метод не соответствует роуту — 405", method: http.MethodGet, target: "/api/students/1", authHeader: "ok", want: http.StatusMethodNotAllowed},
		{name: "GET /api/mark не существует — 405", method: http.MethodGet, target: "/api/mark", authHeader: "ok", want: http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.target, nil)
			if tt.authHeader == "ok" {
				req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
			}
			rec := httptest.NewRecorder()
			a.routes().ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("%s %s = %d, want %d (тело: %s)",
					tt.method, tt.target, rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}
