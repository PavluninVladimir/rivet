// Package blob — объектное хранилище тяжёлых артефактов (транскрипты сессий).
// В Postgres хранятся только ссылки (решение define-rivet-architecture).
package blob

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Store struct {
	client *minio.Client
	bucket string
}

func New(endpoint, accessKey, secretKey, bucket string, useSSL bool) (*Store, error) {
	c, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, err
	}
	return &Store{client: c, bucket: bucket}, nil
}

// EnsureBucket создаёт bucket, если его нет (dev-удобство).
func (s *Store) EnsureBucket(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{})
}

// Put сохраняет объект и возвращает ссылку вида s3://bucket/key.
func (s *Store) Put(ctx context.Context, key string, data []byte) (string, error) {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "text/plain; charset=utf-8"})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("s3://%s/%s", s.bucket, key), nil
}

// Get читает объект по ссылке вида s3://bucket/key (transcript_ref из БД).
// Ссылка обязана указывать на bucket этого store.
func (s *Store) Get(ctx context.Context, ref string) ([]byte, error) {
	key, err := refKey(ref, s.bucket)
	if err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = obj.Close() }()
	return io.ReadAll(obj)
}

// refKey извлекает ключ объекта из ссылки s3://bucket/key, проверяя bucket.
func refKey(ref, bucket string) (string, error) {
	key, ok := strings.CutPrefix(ref, "s3://"+bucket+"/")
	if !ok || key == "" {
		return "", fmt.Errorf("ссылка %q не из bucket %q", ref, bucket)
	}
	return key, nil
}
