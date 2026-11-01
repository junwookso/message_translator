package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type analysisCache struct {
	db            *sql.DB
	modelVersion  string
	promptVersion string
}

func parseCacheEnabled(value string) (bool, error) {
	switch value {
	case "", "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("CACHE_ENABLED must be true or false")
	}
}

func openConfiguredCache(enabled bool, path, modelVersion, promptVersion string, identity ...string) (*analysisCache, error) {
	if !enabled {
		return nil, nil
	}
	if len(identity) == 0 {
		return openCache(path, modelVersion, promptVersion)
	}
	fingerprint := configurationHash(identity[0])
	cache, err := openCache(path, modelVersion+"_"+fingerprint, promptVersion)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = cache.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cache_configurations (
		fingerprint TEXT PRIMARY KEY, configuration_json TEXT NOT NULL
	)`)
	if err == nil {
		_, err = cache.db.ExecContext(ctx, `INSERT INTO cache_configurations (fingerprint, configuration_json)
			VALUES (?, ?) ON CONFLICT(fingerprint) DO NOTHING`, fingerprint, identity[0])
	}
	if err != nil {
		cache.Close()
		return nil, err
	}
	return cache, nil
}

func openCache(path, modelVersion, promptVersion string) (*analysisCache, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection serializes short DB operations, never model inference.
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, statement := range []string{
		"PRAGMA busy_timeout = 1000",
		`CREATE TABLE IF NOT EXISTS analysis_cache (
			message TEXT NOT NULL,
			model_version TEXT NOT NULL,
			prompt_version TEXT NOT NULL,
			result_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (message, model_version, prompt_version)
		)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &analysisCache{db: db, modelVersion: modelVersion, promptVersion: promptVersion}, nil
}

func (c *analysisCache) Get(ctx context.Context, message string) (analysisResult, bool, error) {
	if c == nil {
		return analysisResult{}, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var encoded string
	err := c.db.QueryRowContext(ctx, `SELECT result_json FROM analysis_cache
		WHERE message = ? AND model_version = ? AND prompt_version = ?`,
		message, c.modelVersion, c.promptVersion).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return analysisResult{}, false, nil
	}
	if err != nil {
		return analysisResult{}, false, err
	}
	result, err := decodeAnalysis(encoded)
	return result, err == nil, err
}

func (c *analysisCache) Put(ctx context.Context, message string, result analysisResult) error {
	if c == nil {
		return nil
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if _, err := decodeAnalysis(string(encoded)); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err = c.db.ExecContext(ctx, `INSERT INTO analysis_cache
		(message, model_version, prompt_version, result_json, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(message, model_version, prompt_version) DO UPDATE SET
		result_json = excluded.result_json, created_at = excluded.created_at`,
		message, c.modelVersion, c.promptVersion, string(encoded), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (c *analysisCache) Close() error {
	return c.db.Close()
}
