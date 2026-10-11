package blob

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestRefKey(t *testing.T) {
	cases := []struct {
		ref, bucket, want string
		wantErr           bool
	}{
		{"s3://rivet/tasks/1/attempt-1-CODING.log", "rivet", "tasks/1/attempt-1-CODING.log", false},
		{"s3://other/tasks/1/a.log", "rivet", "", true}, // чужой bucket
		{"s3://rivet/", "rivet", "", true},              // пустой ключ
		{"tasks/1/a.log", "rivet", "", true},            // не s3-ссылка
		{"", "rivet", "", true},
	}
	for _, c := range cases {
		got, err := refKey(c.ref, c.bucket)
		if c.wantErr != (err != nil) {
			t.Fatalf("refKey(%q, %q): err=%v, ожидалась ошибка=%v", c.ref, c.bucket, err, c.wantErr)
		}
		if got != c.want {
			t.Fatalf("refKey(%q, %q) = %q, ожидалось %q", c.ref, c.bucket, got, c.want)
		}
	}
}

// Round-trip Put/Get через MinIO e2e-стенда; без MinIO тест пропускается
// (паттерн store/testStore: интеграция живёт за реальным сервисом).
func TestPutGetRoundTrip(t *testing.T) {
	endpoint := os.Getenv("RIVET_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:9000"
	}
	s, err := New(endpoint, envDef("RIVET_S3_ACCESS_KEY", "rivet"),
		envDef("RIVET_S3_SECRET_KEY", "rivetsecret"), "rivet-test", false)
	if err != nil {
		t.Skipf("minio недоступен: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.EnsureBucket(ctx); err != nil {
		t.Skipf("minio недоступен: %v", err)
	}
	key := fmt.Sprintf("tests/%d.log", time.Now().UnixNano())
	ref, err := s.Put(ctx, key, []byte("строка транскрипта\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "строка транскрипта\n" {
		t.Fatalf("прочитано %q", got)
	}
	if _, err := s.Get(ctx, "s3://another/"+key); err == nil {
		t.Fatal("чтение по ссылке чужого bucket должно падать")
	}
}

func envDef(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
