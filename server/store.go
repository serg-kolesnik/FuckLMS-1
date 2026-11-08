package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Student — запись о студенте. Используется во всех ответах API:
// пароль в эту структуру не входит никогда.
type Student struct {
	ID         int64     `json:"id"`
	FirstName  string    `json:"firstName"`
	LastName   string    `json:"lastName"`
	StudyGroup string    `json:"studyGroup"`
	LmsLogin   string    `json:"lmsLogin"`
	IsSelected bool      `json:"isSelected"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// studentRecord — Student вместе с паролем; живёт только внутри сервиса
// и используется исключительно для отметки посещаемости.
type studentRecord struct {
	Student
	LmsPassword string
}

// StudentCreate — тело POST /api/students (все поля обязательны).
type StudentCreate struct {
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	StudyGroup  string `json:"studyGroup"`
	LmsLogin    string `json:"lmsLogin"`
	LmsPassword string `json:"lmsPassword"`
}

// StudentUpdate — тело PUT /api/students/{id}. nil = поле не передано, не менять.
type StudentUpdate struct {
	FirstName   *string `json:"firstName"`
	LastName    *string `json:"lastName"`
	StudyGroup  *string `json:"studyGroup"`
	LmsLogin    *string `json:"lmsLogin"`
	LmsPassword *string `json:"lmsPassword"`
	IsSelected  *bool   `json:"isSelected"`
}

var (
	errDuplicateLogin = errors.New("студент с таким lms_login уже существует")
	errNotFound       = errors.New("студент не найден")
)

// Ограничения длины — зеркальное отражение VARCHAR-ов из миграции.
const (
	maxNameLen     = 100
	maxGroupLen    = 50
	maxLoginLen    = 100
	maxPasswordLen = 255
)

// Store — доступ к PostgreSQL через pgxpool.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Ping — короткая проверка БД для /health.
func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.pool.Ping(ctx)
}

const studentColumns = `id, first_name, last_name, study_group, lms_login, is_selected, created_at, updated_at`

func scanStudent(row pgx.Row) (Student, error) {
	var st Student
	err := row.Scan(&st.ID, &st.FirstName, &st.LastName, &st.StudyGroup,
		&st.LmsLogin, &st.IsSelected, &st.CreatedAt, &st.UpdatedAt)
	return st, err
}

// ListStudents возвращает всех студентов по возрастанию id.
func (s *Store) ListStudents(ctx context.Context) ([]Student, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+studentColumns+` FROM students ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	students := []Student{}
	for rows.Next() {
		var st Student
		if err := rows.Scan(&st.ID, &st.FirstName, &st.LastName, &st.StudyGroup,
			&st.LmsLogin, &st.IsSelected, &st.CreatedAt, &st.UpdatedAt); err != nil {
			return nil, err
		}
		students = append(students, st)
	}
	return students, rows.Err()
}

// GetStudent возвращает одного студента без пароля.
func (s *Store) GetStudent(ctx context.Context, id int64) (Student, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+studentColumns+` FROM students WHERE id = $1`, id)
	st, err := scanStudent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Student{}, errNotFound
	}
	return st, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// CreateStudent добавляет студента; конфликт lms_login → errDuplicateLogin.
func (s *Store) CreateStudent(ctx context.Context, in StudentCreate) (Student, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO students (first_name, last_name, study_group, lms_login, lms_password)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+studentColumns,
		in.FirstName, in.LastName, in.StudyGroup, in.LmsLogin, in.LmsPassword)
	st, err := scanStudent(row)
	if isUniqueViolation(err) {
		return Student{}, errDuplicateLogin
	}
	return st, err
}

// UpdateStudent обновляет только переданные поля; пароль не передан — не меняется.
func (s *Store) UpdateStudent(ctx context.Context, id int64, in StudentUpdate) (Student, error) {
	var sets []string
	var args []any
	add := func(column string, value any) {
		sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)+1))
		args = append(args, value)
	}
	if in.FirstName != nil {
		add("first_name", *in.FirstName)
	}
	if in.LastName != nil {
		add("last_name", *in.LastName)
	}
	if in.StudyGroup != nil {
		add("study_group", *in.StudyGroup)
	}
	if in.LmsLogin != nil {
		add("lms_login", *in.LmsLogin)
	}
	if in.LmsPassword != nil {
		add("lms_password", *in.LmsPassword)
	}
	if in.IsSelected != nil {
		add("is_selected", *in.IsSelected)
	}

	if len(sets) == 0 {
		return s.GetStudent(ctx, id)
	}
	add("updated_at", time.Now())

	args = append(args, id)
	row := s.pool.QueryRow(ctx, `
		UPDATE students SET `+strings.Join(sets, ", ")+`
		WHERE id = $`+fmt.Sprint(len(args))+`
		RETURNING `+studentColumns, args...)
	st, err := scanStudent(row)
	if isUniqueViolation(err) {
		return Student{}, errDuplicateLogin
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Student{}, errNotFound
	}
	return st, err
}

// DeleteStudent удаляет студента; false = не было такого id.
func (s *Store) DeleteStudent(ctx context.Context, id int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM students WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// StudentsByIDs возвращает студентов с паролями по списку id (только для отметки).
func (s *Store) StudentsByIDs(ctx context.Context, ids []int64) ([]studentRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, first_name, last_name, study_group, lms_login, is_selected, created_at, updated_at, lms_password
		FROM students WHERE id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return nil, err
	}
	return scanRecords(rows)
}

// SelectedStudents возвращает всех студентов с is_selected = true (только для отметки).
func (s *Store) SelectedStudents(ctx context.Context) ([]studentRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, first_name, last_name, study_group, lms_login, is_selected, created_at, updated_at, lms_password
		FROM students WHERE is_selected = TRUE ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanRecords(rows)
}

func scanRecords(rows pgx.Rows) ([]studentRecord, error) {
	defer rows.Close()
	records := []studentRecord{}
	for rows.Next() {
		var rec studentRecord
		if err := rows.Scan(&rec.ID, &rec.FirstName, &rec.LastName, &rec.StudyGroup,
			&rec.LmsLogin, &rec.IsSelected, &rec.CreatedAt, &rec.UpdatedAt, &rec.LmsPassword); err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

// validateStudent проверяет непустоту и длину полей, возвращает текст ошибки или "".
func validateStudent(firstName, lastName, studyGroup, lmsLogin, lmsPassword string) string {
	for _, v := range []struct {
		label string
		value string
		max   int
	}{
		{"firstName", firstName, maxNameLen},
		{"lastName", lastName, maxNameLen},
		{"studyGroup", studyGroup, maxGroupLen},
		{"lmsLogin", lmsLogin, maxLoginLen},
		{"lmsPassword", lmsPassword, maxPasswordLen},
	} {
		if strings.TrimSpace(v.value) == "" {
			return fmt.Sprintf("поле %s обязательно", v.label)
		}
		if utf8.RuneCountInString(v.value) > v.max {
			return fmt.Sprintf("поле %s длиннее %d символов", v.label, v.max)
		}
	}
	return ""
}
