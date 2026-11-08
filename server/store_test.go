package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestValidateStudent(t *testing.T) {
	// validStudent — эталонный набор полей; в кейсах портим одно поле за раз.
	valid := func() map[string]string {
		return map[string]string{
			"firstName":   "Иван",
			"lastName":    "Иванов",
			"studyGroup":  "101-А",
			"lmsLogin":    "ivanov",
			"lmsPassword": "secret",
		}
	}

	tests := []struct {
		name      string
		mutate    func(fields map[string]string)
		wantMsg   string
		wantEmpty bool // true — ошибок быть не должно
	}{
		{
			name:      "все поля корректны",
			mutate:    func(map[string]string) {},
			wantEmpty: true,
		},
		{
			name:    "пустой firstName",
			mutate:  func(f map[string]string) { f["firstName"] = "" },
			wantMsg: "поле firstName обязательно",
		},
		{
			name:    "firstName из одних пробелов",
			mutate:  func(f map[string]string) { f["firstName"] = "   " },
			wantMsg: "поле firstName обязательно",
		},
		{
			name:    "пустой lastName",
			mutate:  func(f map[string]string) { f["lastName"] = "" },
			wantMsg: "поле lastName обязательно",
		},
		{
			name:    "пустой studyGroup",
			mutate:  func(f map[string]string) { f["studyGroup"] = "" },
			wantMsg: "поле studyGroup обязательно",
		},
		{
			name:    "пустой lmsLogin",
			mutate:  func(f map[string]string) { f["lmsLogin"] = "" },
			wantMsg: "поле lmsLogin обязательно",
		},
		{
			name:    "пустой lmsPassword",
			mutate:  func(f map[string]string) { f["lmsPassword"] = "" },
			wantMsg: "поле lmsPassword обязательно",
		},
		{
			name:    "firstName длиннее 100 символов (в рунах, не байтах)",
			mutate:  func(f map[string]string) { f["firstName"] = strings.Repeat("а", 101) },
			wantMsg: "поле firstName длиннее 100 символов",
		},
		{
			name:    "lastName длиннее 100 символов",
			mutate:  func(f map[string]string) { f["lastName"] = strings.Repeat("b", 101) },
			wantMsg: "поле lastName длиннее 100 символов",
		},
		{
			name:    "studyGroup длиннее 50 символов",
			mutate:  func(f map[string]string) { f["studyGroup"] = strings.Repeat("g", 51) },
			wantMsg: "поле studyGroup длиннее 50 символов",
		},
		{
			name:    "lmsLogin длиннее 100 символов",
			mutate:  func(f map[string]string) { f["lmsLogin"] = strings.Repeat("l", 101) },
			wantMsg: "поле lmsLogin длиннее 100 символов",
		},
		{
			name:    "lmsPassword длиннее 255 символов",
			mutate:  func(f map[string]string) { f["lmsPassword"] = strings.Repeat("p", 256) },
			wantMsg: "поле lmsPassword длиннее 255 символов",
		},
		{
			name:      "ровно на границе длины — допустимо",
			mutate:    func(f map[string]string) { f["firstName"] = strings.Repeat("а", 100) },
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := valid()
			tt.mutate(f)
			got := validateStudent(f["firstName"], f["lastName"], f["studyGroup"],
				f["lmsLogin"], f["lmsPassword"])
			switch {
			case tt.wantEmpty && got != "":
				t.Errorf("validateStudent() = %q, ожидается пустая строка", got)
			case !tt.wantEmpty && got != tt.wantMsg:
				t.Errorf("validateStudent() = %q, want %q", got, tt.wantMsg)
			}
		})
	}
}

func TestIsUniqueViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "ошибка уникальности 23505", err: &pgconn.PgError{Code: "23505"}, want: true},
		{name: "другая ошибка PostgreSQL", err: &pgconn.PgError{Code: "23503"}, want: false},
		{name: "обычная ошибка", err: errors.New("что-то не так"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUniqueViolation(tt.err); got != tt.want {
				t.Errorf("isUniqueViolation(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestStudentJSONContract(t *testing.T) {
	st := Student{
		ID:         7,
		FirstName:  "Иван",
		LastName:   "Иванов",
		StudyGroup: "101-А",
		LmsLogin:   "ivanov",
		IsSelected: true,
		CreatedAt:  time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
		UpdatedAt:  time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("Marshal(Student) неожиданная ошибка: %v", err)
	}

	// Пароль не должен просачиваться ни в каком виде — контракт openapi.yaml.
	if strings.Contains(strings.ToLower(string(raw)), "password") {
		t.Errorf("в JSON Student попало поле пароля: %s", raw)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal неожиданная ошибка: %v", err)
	}
	wantKeys := []string{"id", "firstName", "lastName", "studyGroup", "lmsLogin", "isSelected", "createdAt", "updatedAt"}
	if len(decoded) != len(wantKeys) {
		t.Errorf("в JSON Student %d ключей (%v), want %d", len(decoded), decoded, len(wantKeys))
	}
	for _, key := range wantKeys {
		if _, ok := decoded[key]; !ok {
			t.Errorf("в JSON Student нет ключа %q", key)
		}
	}
}
