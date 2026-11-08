package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// errorPayload — единый формат ошибок (openapi.yaml, схема Error).
type errorPayload struct {
	Message string `json:"message"`
}

// writeJSON отправляет payload с кодом status.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorPayload{Message: message})
}

// auth — middleware bearer-авторизации для всех ручек, кроме /health.
func (a *app) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" ||
			subtle.ConstantTimeCompare([]byte(token), []byte(a.cfg.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "требуется Authorization: Bearer <token>")
			return
		}
		next(w, r)
	}
}

// handleHealth — проверка живости; без авторизации (openapi.yaml, security: []).
func (a *app) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"status": "degraded", "database": "down"})
		return
	}
	writeJSON(w, http.StatusOK,
		map[string]string{"status": "ok", "database": "up"})
}

// handleListStudents — GET /api/students: список без паролей.
func (a *app) handleListStudents(w http.ResponseWriter, r *http.Request) {
	students, err := a.store.ListStudents(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ошибка БД: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, students)
}

// handleCreateStudent — POST /api/students.
func (a *app) handleCreateStudent(w http.ResponseWriter, r *http.Request) {
	var in StudentCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "неверное тело запроса: "+err.Error())
		return
	}
	if msg := validateStudent(in.FirstName, in.LastName, in.StudyGroup, in.LmsLogin, in.LmsPassword); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	student, err := a.store.CreateStudent(r.Context(), in)
	switch {
	case errors.Is(err, errDuplicateLogin):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "ошибка БД: "+err.Error())
	default:
		writeJSON(w, http.StatusCreated, student)
	}
}

// parseStudentId разбирает {id} из пути.
func parseStudentId(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// handleUpdateStudent — PUT /api/students/{id}; пустое поле lmsPassword = не менять.
func (a *app) handleUpdateStudent(w http.ResponseWriter, r *http.Request) {
	id, err := parseStudentId(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "некорректный id студента")
		return
	}
	var in StudentUpdate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "неверное тело запроса: "+err.Error())
		return
	}

	// Валидируем только переданные поля (обязательные при создании здесь опциональны).
	check := func(label string, value *string, max int) string {
		if value == nil {
			return ""
		}
		if strings.TrimSpace(*value) == "" {
			return "поле " + label + " не может быть пустым"
		}
		if runeCount(*value) > max {
			return "поле " + label + " длиннее " + strconv.Itoa(max) + " символов"
		}
		return ""
	}
	for _, c := range []struct {
		label string
		value *string
		max   int
	}{
		{"firstName", in.FirstName, maxNameLen},
		{"lastName", in.LastName, maxNameLen},
		{"studyGroup", in.StudyGroup, maxGroupLen},
		{"lmsLogin", in.LmsLogin, maxLoginLen},
		{"lmsPassword", in.LmsPassword, maxPasswordLen},
	} {
		if msg := check(c.label, c.value, c.max); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}

	student, err := a.store.UpdateStudent(r.Context(), id, in)
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errDuplicateLogin):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "ошибка БД: "+err.Error())
	default:
		writeJSON(w, http.StatusOK, student)
	}
}

// runeCount — длина строки в символах (не байтах), ограничения совпадают с VARCHAR.
func runeCount(s string) int { return utf8.RuneCountInString(s) }

// handleDeleteStudent — DELETE /api/students/{id}.
func (a *app) handleDeleteStudent(w http.ResponseWriter, r *http.Request) {
	id, err := parseStudentId(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "некорректный id студента")
		return
	}
	deleted, err := a.store.DeleteStudent(r.Context(), id)
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, "ошибка БД: "+err.Error())
	case !deleted:
		writeError(w, http.StatusNotFound, errNotFound.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
