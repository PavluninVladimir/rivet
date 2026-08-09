package runner

import (
	"os"
	"path/filepath"
	"sort"
	"sync"

	"google.golang.org/protobuf/proto"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// outbox — файловый журнал недоставленных сообщений: сообщение лежит на диске
// до Ack от control plane, при переподключении отправляется повторно
// (доставка at-least-once, дедупликация по msg_id на приёме).
type outbox struct {
	dir string
	mu  sync.Mutex
}

func newOutbox(dir string) (*outbox, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &outbox{dir: dir}, nil
}

func (o *outbox) path(msgID string) string {
	return filepath.Join(o.dir, msgID+".pb")
}

func (o *outbox) put(m *pb.RunnerMsg) {
	// Heartbeat не журналируется — потеря безвредна.
	if _, ok := m.Kind.(*pb.RunnerMsg_Heartbeat); ok {
		return
	}
	raw, err := proto.Marshal(m)
	if err != nil {
		return
	}
	o.mu.Lock()
	_ = os.WriteFile(o.path(m.MsgId), raw, 0o644)
	o.mu.Unlock()
}

func (o *outbox) ack(msgID string) {
	o.mu.Lock()
	_ = os.Remove(o.path(msgID))
	o.mu.Unlock()
}

// pending — недоставленные сообщения в порядке создания файлов.
func (o *outbox) pending() []*pb.RunnerMsg {
	o.mu.Lock()
	defer o.mu.Unlock()
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool {
		fi, _ := entries[i].Info()
		fj, _ := entries[j].Info()
		if fi == nil || fj == nil {
			return entries[i].Name() < entries[j].Name()
		}
		return fi.ModTime().Before(fj.ModTime())
	})
	var out []*pb.RunnerMsg
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(o.dir, e.Name()))
		if err != nil {
			continue
		}
		var m pb.RunnerMsg
		if proto.Unmarshal(raw, &m) == nil {
			out = append(out, &m)
		}
	}
	return out
}
