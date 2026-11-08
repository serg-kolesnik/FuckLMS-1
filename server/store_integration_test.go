//go:build integration

package main

// Интеграционные тесты Store против настоящего PostgreSQL.
// Схема пересоздаётся из migrations/0001_init.sql, поэтому БД обязана быть
// одноразовой (никаких продакшен-данных).
//
// Запуск:
//
//	FLMS_TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/fucklms_test \
//	  go test -tags=integration ./...

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// newIntegrationStore пересоздаёт таблицу students из миграции и возвращает Store.
func newIntegrationStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("FLMS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FLMS_TEST_DATABASE_URL не задан — интеграционные тесты пропущены")
	}

	ctx := context.Background()
	store, err := NewStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)

	// Чистая схема: DROP + миграция из файла.
	schema, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("не прочитан файл миграции: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `DROP TABLE IF EXISTS students`); err != nil {
		t.Fatalf("DROP TABLE: %v", err)
	}
	if _, err := store.pool.Exec(ctx, string(schema)); err != nil {
		t.Fatalf("миграция: %v", err)
	}
	return store
}

func TestNewStore_BadDSN(t *testing.T) {
	// Порт 1 на localhost: соединение отклоняется сразу.
	if _, err := NewStore(context.Background(), "postgres://127.0.0.1:1/nope?sslmode=disable"); err == nil {
		t.Error("NewStore с недоступной БД должен вернуть ошибку")
	}
}

func TestNewStore_UnparsableDSN(t *testing.T) {
	// Строка подключения не парсится — ошибка ещё до попытки соединения.
	if _, err := NewStore(context.Background(), "postgres://%zz"); err == nil {
		t.Error("NewStore с непарсируемым DSN должен вернуть ошибку")
	}
}

func TestStore_SchemaMissing(t *testing.T) {
	store := newIntegrationStore(t)
	ctx := context.Background()

	// Ломаем схему: все методы должны вернуть ошибку, а не паниковать.
	if _, err := store.pool.Exec(ctx, `DROP TABLE students`); err != nil {
		t.Fatalf("DROP TABLE: %v", err)
	}
	ivans := StudentCreate{
		FirstName: "Иван", LastName: "Иванов", StudyGroup: "101-А",
		LmsLogin: "ivanov", LmsPassword: "p",
	}
	if _, err := store.ListStudents(ctx); err == nil {
		t.Error("ListStudents без таблицы должен вернуть ошибку")
	}
	if _, err := store.CreateStudent(ctx, ivans); err == nil {
		t.Error("CreateStudent без таблицы должен вернуть ошибку")
	}
	if _, err := store.StudentsByIDs(ctx, []int64{1}); err == nil {
		t.Error("StudentsByIDs без таблицы должен вернуть ошибку")
	}
	if _, err := store.SelectedStudents(ctx); err == nil {
		t.Error("SelectedStudents без таблицы должен вернуть ошибку")
	}
	if _, err := store.DeleteStudent(ctx, 1); err == nil {
		t.Error("DeleteStudent без таблицы должен вернуть ошибку")
	}
}

func TestStore_PostgreSQL(t *testing.T) {
	store := newIntegrationStore(t)
	ctx := context.Background()
	base := StudentCreate{
		FirstName:   "Иван",
		LastName:    "Иванов",
		StudyGroup:  "101-А",
		LmsLogin:    "ivanov",
		LmsPassword: "secret-1",
	}

	created := t.Run("create", func(t *testing.T) {
		st, err := store.CreateStudent(ctx, base)
		if err != nil {
			t.Fatalf("CreateStudent: %v", err)
		}
		if st.ID == 0 || !st.IsSelected {
			t.Errorf("создан %v+..., want id != 0, isSelected = true", st.ID)
		}
		if st.CreatedAt.IsZero() || st.UpdatedAt.IsZero() {
			t.Errorf("timestamp-ы не проставлены: %v / %v", st.CreatedAt, st.UpdatedAt)
		}
	})
	if !created {
		t.Fatal("создание студента не удалось — продолжать сценарий бессмысленно")
	}

	t.Run("дубликат lms_login", func(t *testing.T) {
		_, err := store.CreateStudent(ctx, base)
		if !errors.Is(err, errDuplicateLogin) {
			t.Errorf("CreateStudent дубль = %v, want errDuplicateLogin", err)
		}
	})

	var ivanovID int64
	t.Run("list по возрастанию id", func(t *testing.T) {
		if _, err := store.CreateStudent(ctx, StudentCreate{
			FirstName: "Пётр", LastName: "Петров", StudyGroup: "101-А",
			LmsLogin: "petrov", LmsPassword: "secret-2",
		}); err != nil {
			t.Fatalf("CreateStudent: %v", err)
		}
		list, err := store.ListStudents(ctx)
		if err != nil {
			t.Fatalf("ListStudents: %v", err)
		}
		if len(list) != 2 || list[0].LmsLogin != "ivanov" || list[1].LmsLogin != "petrov" {
			t.Fatalf("ListStudents = %+v, want [ivanov petrov]", list)
		}
		ivanovID = list[0].ID
	})

	t.Run("get несуществующего — errNotFound", func(t *testing.T) {
		if _, err := store.GetStudent(ctx, 1_000_000); !errors.Is(err, errNotFound) {
			t.Errorf("GetStudent = %v, want errNotFound", err)
		}
	})

	strPtr := func(s string) *string { return &s }
	boolPtr := func(b bool) *bool { return &b }

	t.Run("update частичный: пароль не передан — не меняется", func(t *testing.T) {
		updated, err := store.UpdateStudent(ctx, ivanovID, StudentUpdate{StudyGroup: strPtr("202-Б")})
		if err != nil {
			t.Fatalf("UpdateStudent: %v", err)
		}
		if updated.StudyGroup != "202-Б" {
			t.Errorf("studyGroup = %q, want 202-Б", updated.StudyGroup)
		}
		records, err := store.StudentsByIDs(ctx, []int64{ivanovID})
		if err != nil || len(records) != 1 {
			t.Fatalf("StudentsByIDs = %+v, %v", records, err)
		}
		if records[0].LmsPassword != "secret-1" {
			t.Errorf("пароль изменился без явного lmsPassword: %q", records[0].LmsPassword)
		}
	})

	t.Run("update пароля и isSelected", func(t *testing.T) {
		_, err := store.UpdateStudent(ctx, ivanovID, StudentUpdate{
			LmsPassword: strPtr("secret-3"),
			IsSelected:  boolPtr(false),
		})
		if err != nil {
			t.Fatalf("UpdateStudent: %v", err)
		}
		records, err := store.StudentsByIDs(ctx, []int64{ivanovID})
		if err != nil || len(records) != 1 {
			t.Fatalf("StudentsByIDs: %v", err)
		}
		if records[0].LmsPassword != "secret-3" {
			t.Errorf("пароль = %q, want secret-3", records[0].LmsPassword)
		}
		if records[0].IsSelected {
			t.Error("isSelected = true, want false")
		}
	})

	t.Run("update пустым телом возвращает текущего", func(t *testing.T) {
		st, err := store.UpdateStudent(ctx, ivanovID, StudentUpdate{})
		if err != nil {
			t.Fatalf("UpdateStudent: %v", err)
		}
		if st.ID != ivanovID || st.StudyGroup != "202-Б" {
			t.Errorf("UpdateStudent{} = %+v, want текущее состояние", st)
		}
	})

	t.Run("update несуществующего — errNotFound", func(t *testing.T) {
		_, err := store.UpdateStudent(ctx, 1_000_000, StudentUpdate{FirstName: strPtr("X")})
		if !errors.Is(err, errNotFound) {
			t.Errorf("UpdateStudent = %v, want errNotFound", err)
		}
	})

	t.Run("update логина на существующий — errDuplicateLogin", func(t *testing.T) {
		_, err := store.UpdateStudent(ctx, ivanovID, StudentUpdate{LmsLogin: strPtr("petrov")})
		if !errors.Is(err, errDuplicateLogin) {
			t.Errorf("UpdateStudent = %v, want errDuplicateLogin", err)
		}
	})

	t.Run("selected — только выбранные, с паролями", func(t *testing.T) {
		records, err := store.SelectedStudents(ctx)
		if err != nil {
			t.Fatalf("SelectedStudents: %v", err)
		}
		if len(records) != 1 || records[0].LmsLogin != "petrov" || records[0].LmsPassword != "secret-2" {
			t.Fatalf("SelectedStudents = %+v, want только petrov с паролем", records)
		}
	})

	t.Run("students by ids — подмножество по возрастанию", func(t *testing.T) {
		records, err := store.StudentsByIDs(ctx, []int64{ivanovID, 999, ivanovID})
		if err != nil {
			t.Fatalf("StudentsByIDs: %v", err)
		}
		if len(records) != 1 || records[0].LmsLogin != "ivanov" {
			t.Fatalf("StudentsByIDs = %+v, want только ivanov", records)
		}
	})

	t.Run("delete", func(t *testing.T) {
		deleted, err := store.DeleteStudent(ctx, ivanovID)
		if err != nil || !deleted {
			t.Fatalf("DeleteStudent = %v, %v; want true, nil", deleted, err)
		}
		again, err := store.DeleteStudent(ctx, ivanovID)
		if err != nil || again {
			t.Fatalf("повторный DeleteStudent = %v, %v; want false, nil", again, err)
		}
	})

	t.Run("ping", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := store.Ping(ctx); err != nil {
			t.Errorf("Ping: %v", err)
		}
	})
}
