package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"pimg/internal/dbx"
	"pimg/internal/storage"
)

func (s *Service) mediaReplicaSources(ctx context.Context, f *dbx.File) ([]replicaSource, error) {
	replica, err := s.MediaManifest(f)
	if err != nil {
		return nil, err
	}
	var out []replicaSource
	for _, part := range replica.Parts {
		var ref storage.Reference
		if err = json.Unmarshal([]byte(part.Reference), &ref); err != nil {
			return nil, err
		}
		var open func(context.Context) (io.ReadCloser, error)
		if s.telegramInstance(replica.InstanceID) && s.UserTG != nil {
			object, e := s.telegramObject(ref.Key)
			if e != nil {
				return nil, e
			}
			open = func(c context.Context) (io.ReadCloser, error) { return s.openTelegramWire(c, object) }
		} else {
			backend, e := s.archiveBackend(replica.InstanceID)
			if e != nil {
				return nil, e
			}
			open = func(c context.Context) (io.ReadCloser, error) { return backend.Open(c, ref) }
		}
		input, e := open(ctx)
		if e != nil {
			return nil, e
		}
		h := sha256New()
		n, e := io.Copy(h, contextReaderForCache{ctx, input})
		e2 := input.Close()
		if e != nil {
			return nil, e
		}
		if e2 != nil {
			return nil, e2
		}
		name, contentType := MediaPartName(f, part), part.MIME
		if replica.Encrypted {
			name = fmt.Sprintf("%s.part-%05d.pimg.png", f.ID, part.Index)
			contentType = "image/png"
		}
		out = append(out, replicaSource{ReplicaObject: storage.ReplicaObject{Name: name, Size: n, SHA256: hexOf(h), MIME: contentType}, open: open})
	}
	return out, nil
}
