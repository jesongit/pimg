package store

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"pimg/internal/cryptox"
	"pimg/internal/dbx"
)

func mediaCipherPrefix(id string) string { return sha256Hex([]byte(id)) + ".media-" }

func (s *Service) mediaCipherPath(f *dbx.File, r dbx.MediaReplica, p dbx.MediaPart) string {
	return filepath.Join(s.cipherDir, mediaCipherPrefix(f.ID)+sha256Hex([]byte(r.InstanceID+"/"+strconv.Itoa(p.Index)+"/"+p.SHA256))+".bin")
}

// Persist the visible cover before creating new ciphertext. A retry after a
// lost acknowledgement must resend the same complete PNG, even if previews
// change or the original upload working file disappears in the meantime.
func (s *Service) prepareMediaCover(ctx context.Context, f *dbx.File, r dbx.MediaReplica, p dbx.MediaPart, sourcePath string) ([]byte, bool, error) {
	cipherPath := s.mediaCipherPath(f, r, p)
	coverPath := cipherPath + ".cover.png"
	if data, err := os.ReadFile(coverPath); err == nil {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 256 || cfg.Height > 256 {
			return nil, false, errors.New("续传封面损坏，保留原续传数据以避免重复上传")
		}
		if _, _, err = image.Decode(bytes.NewReader(data)); err != nil {
			return nil, false, err
		}
		return data, false, nil
	} else if !os.IsNotExist(err) {
		return nil, false, err
	}
	if _, err := os.Stat(cipherPath); err == nil {
		// Versions before blurred v2 covers could already have sent these
		// encrypted bytes using the original black PNG and random ID. Finish
		// that resumable write with its original representation.
		return privacyPlaceholder(), true, nil
	} else if !os.IsNotExist(err) {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	preview, _ := s.carrierPreviewFromSource(ctx, f, sourcePath)
	out, err := os.CreateTemp(s.cipherDir, "media-cover-*.part")
	if err != nil {
		return nil, false, err
	}
	defer os.Remove(out.Name())
	_ = out.Chmod(0600)
	_, err = out.Write(preview)
	err = errors.Join(err, out.Close())
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = os.Rename(out.Name(), coverPath)
	}
	return preview, false, err
}

func mediaCoverThumbnail(preview []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(preview))
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err = jpeg.Encode(&output, img, &jpeg.Options{Quality: 70}); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// Keep identical ciphertext until readback succeeds. In particular, Telegram
// retries with a stable random ID must resend the exact same encrypted bytes
// after a lost acknowledgement or a service restart.
func (s *Service) prepareMediaCipher(ctx context.Context, f *dbx.File, r dbx.MediaReplica, p dbx.MediaPart, plain io.Reader) (*os.File, int64, string, error) {
	path := s.mediaCipherPath(f, r, p)
	key, err := s.Master.UnwrapFileKey(f.KeyWrapped, f.KeyNonce)
	if err != nil {
		return nil, 0, "", err
	}
	if existing, e := os.Open(path); e == nil {
		decoded, e := cryptox.DecryptReader(existing, key)
		if e == nil {
			h := sha256New()
			var n int64
			n, e = io.Copy(h, io.LimitReader(contextReaderForCache{ctx, decoded}, p.Size+1))
			if e == nil && (n != p.Size || hexOf(h) != p.SHA256) {
				e = errors.New("续传密文与来源不匹配")
			}
		}
		if e != nil {
			existing.Close()
			return nil, 0, "", e
		}
		if _, e = existing.Seek(0, io.SeekStart); e != nil {
			existing.Close()
			return nil, 0, "", e
		}
		h := sha256New()
		n, e := io.Copy(h, contextReaderForCache{ctx, existing})
		if e == nil {
			_, e = existing.Seek(0, io.SeekStart)
		}
		if e != nil {
			existing.Close()
			return nil, 0, "", e
		}
		return existing, n, hexOf(h), nil
	} else if !os.IsNotExist(e) {
		return nil, 0, "", e
	}
	encoded, err := cryptox.EncryptReader(plain, key)
	if err != nil {
		return nil, 0, "", err
	}
	if closer, ok := encoded.(io.Closer); ok {
		defer closer.Close()
	}
	out, err := os.CreateTemp(s.cipherDir, "media-cipher-*.part")
	if err != nil {
		return nil, 0, "", err
	}
	defer os.Remove(out.Name())
	_ = out.Chmod(0600)
	h := sha256New()
	n, err := io.Copy(io.MultiWriter(out, h), contextReaderForCache{ctx, encoded})
	err = errors.Join(err, out.Close())
	if err != nil {
		return nil, 0, "", err
	}
	if err = os.Rename(out.Name(), path); err != nil {
		return nil, 0, "", err
	}
	file, err := os.Open(path)
	return file, n, hexOf(h), err
}

func (s *Service) removeMediaCipherWork(id string) error {
	var paths []string
	for _, suffix := range []string{"*.bin", "*.bin.cover.png"} {
		matched, err := filepath.Glob(filepath.Join(s.cipherDir, mediaCipherPrefix(id)+suffix))
		if err != nil {
			return err
		}
		paths = append(paths, matched...)
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
