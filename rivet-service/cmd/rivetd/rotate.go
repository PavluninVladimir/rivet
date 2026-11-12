package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/PavluninVladimir/rivet/internal/config"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// rotateKey выполняет rivetd -rotate-key: перешифровывает секреты установки с
// прежнего ключа (RIVET_SECRET_KEY_OLD) на новый (RIVET_SECRET_KEY) одной
// транзакцией, с событием в event log (спека access-policy «Безопасные
// умолчания установки»). Прежним ключом может быть и опубликованный: ради
// ухода с него команда и нужна. Новый опубликованным быть не может.
func rotateKey(ctx context.Context, cfg config.Config) error {
	oldBox, newBox, err := rotateBoxes(cfg)
	if err != nil {
		return err
	}
	if err := store.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	st, err := store.NewNamed(ctx, cfg.DatabaseURL, store.RotateApp)
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := st.RotateSecrets(ctx, oldBox, newBox)
	if err != nil {
		return err
	}
	slog.Info("секреты перешифрованы новым ключом: запускайте rivetd с RIVET_SECRET_KEY, прежний ключ больше не нужен",
		"scm_credentials", res.Credentials, "connection_keys", res.ConnectionKeys, "infra_tokens", res.InfraTokens,
		"connection_headers", res.ConnectionHeaders, "already_new", res.AlreadyNew)
	return nil
}

// rotateBoxes проверяет пару ключей до обращения к базе.
func rotateBoxes(cfg config.Config) (oldBox, newBox *secretbox.Box, err error) {
	if cfg.SecretKeyOld == "" {
		return nil, nil, errors.New("задайте прежний ключ, которым зашифрованы секреты, в RIVET_SECRET_KEY_OLD")
	}
	if cfg.SecretKey == "" {
		return nil, nil, errors.New("задайте новый ключ в RIVET_SECRET_KEY: openssl rand -base64 32")
	}
	if config.IsPublishedSecretKey(cfg.SecretKey) {
		return nil, nil, fmt.Errorf("%w: новый ключ RIVET_SECRET_KEY опубликован в документации или примерах, сгенерируйте свой: openssl rand -base64 32", config.ErrPublished)
	}
	if oldBox, err = secretbox.New(cfg.SecretKeyOld); err != nil {
		return nil, nil, fmt.Errorf("RIVET_SECRET_KEY_OLD: %w", err)
	}
	if newBox, err = secretbox.New(cfg.SecretKey); err != nil {
		return nil, nil, fmt.Errorf("RIVET_SECRET_KEY: %w", err)
	}
	// Тот же ключ в другой записи base64 тоже считается совпадением. Проверка по
	// тому, читает ли новый ключ то, что зашифровал прежний.
	probe, err := oldBox.Seal("rotate-key-probe")
	if err != nil {
		return nil, nil, err
	}
	if _, err := newBox.Open(probe); err == nil {
		return nil, nil, errors.New("RIVET_SECRET_KEY_OLD и RIVET_SECRET_KEY совпадают: перешифровывать нечем")
	}
	return oldBox, newBox, nil
}
