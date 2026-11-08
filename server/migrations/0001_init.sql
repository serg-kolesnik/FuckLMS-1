-- Миграция 0001: таблица студентов (ADR 0001).
-- Единственное хранилище аккаунтов FuckLMS; применяется вручную:
--   psql "$DATABASE_URL" -f migrations/0001_init.sql

CREATE TABLE IF NOT EXISTS students (
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

CREATE INDEX IF NOT EXISTS idx_students_group ON students (study_group);
