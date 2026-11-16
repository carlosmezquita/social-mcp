package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const MaxMedia = 64 << 20

func (s *Store) media(ctx context.Context, owner, key string, admin bool) (Media, error) {
	var m Media
	err := s.DB.QueryRowContext(ctx, "SELECT id,mime,size,sha256 FROM media WHERE id=? AND (owner=? OR ?)", key, owner, admin).Scan(&m.ID, &m.MIME, &m.Size, &m.SHA)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrMissing
	}
	if err == nil {
		m.Path = filepath.Join(s.mediaDir, m.ID)
	}
	return m, err
}

func (s *Store) upload(ctx context.Context, p Principal, r io.Reader) (Media, error) {
	m := Media{ID: id()}
	m.Path = filepath.Join(s.mediaDir, m.ID)
	f, err := os.OpenFile(m.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return m, err
	}
	saved := false
	defer func() {
		f.Close()
		if !saved {
			os.Remove(m.Path)
		}
	}()
	var prefix [512]byte
	n, err := io.ReadFull(r, prefix[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return m, invalid("empty or unreadable file")
	}
	m.MIME = http.DetectContentType(prefix[:n])
	switch m.MIME {
	case "image/jpeg", "image/png", "video/mp4":
	default:
		return m, invalid("upload JPEG, PNG or MP4 bytes")
	}
	hash := sha256.New()
	writer := io.MultiWriter(f, hash)
	if _, err = writer.Write(prefix[:n]); err != nil {
		return m, err
	}
	copied, err := io.Copy(writer, io.LimitReader(r, MaxMedia+1-int64(n)))
	if err != nil {
		return m, err
	}
	m.Size = copied + int64(n)
	if m.Size > MaxMedia {
		return m, invalid("media exceeds 64 MiB")
	}
	if err = f.Sync(); err != nil {
		return m, err
	}
	if err = f.Close(); err != nil {
		return m, err
	}
	dir, err := os.Open(s.mediaDir)
	if err != nil {
		return m, err
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		return m, err
	}
	m.SHA = hex.EncodeToString(hash.Sum(nil))
	_, err = s.DB.ExecContext(ctx, "INSERT INTO media VALUES(?,?,?,?,?,?)", m.ID, p.ID, m.MIME, m.Size, m.SHA, time.Now().UnixMilli())
	saved = err == nil
	return m, err
}

func (a *Service) mediaMAC(key, expiry string) string {
	mac := hmac.New(sha256.New, a.C.Key)
	mac.Write([]byte("media:" + key + ":" + expiry))
	return hex.EncodeToString(mac.Sum(nil))
}
func (a *Service) mediaURL(key string) string {
	expiry := strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10)
	return a.C.Public + "/public/media/" + key + "?" + url.Values{"expires": {expiry}, "sig": {a.mediaMAC(key, expiry)}}.Encode()
}
func (a *Service) serveMedia(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	key, expiry := r.PathValue("id"), q.Get("expires")
	deadline, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || time.Now().Unix() >= deadline || !hmac.Equal([]byte(q.Get("sig")), []byte(a.mediaMAC(key, expiry))) {
		http.NotFound(w, r)
		return
	}
	m, err := a.S.media(r.Context(), "", key, true)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(m.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", m.MIME)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, m.ID, time.Time{}, f)
}

// Clean only never-used uploads. Referenced assets remain available for history and restore.
func (s *Store) Cleanup(ctx context.Context, age time.Duration) (int, error) {
	rows, err := s.DB.QueryContext(ctx, `DELETE FROM media WHERE created<? AND NOT EXISTS (
  SELECT 1 FROM deliveries WHERE json_extract(CAST(payload AS TEXT),'$.media_id')=media.id
   OR json_extract(CAST(payload AS TEXT),'$.custom_preview.image_id')=media.id
 ) RETURNING id`, time.Now().Add(-age).UnixMilli())
	if err != nil {
		return 0, err
	}
	var keys []string
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			rows.Close()
			return 0, err
		}
		keys = append(keys, key)
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	for _, key := range keys {
		if err = os.Remove(filepath.Join(s.mediaDir, key)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
	}
	return len(keys), nil
}

func (s *Store) CopySnapshotMedia(ctx context.Context, source string) error {
	if !filepath.IsAbs(source) || filepath.Clean(source) == filepath.Clean(s.mediaDir) {
		return invalid("a separate absolute source media directory is required")
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT id FROM media")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return err
		}
		if !identifier.MatchString(key) {
			return errors.New("invalid stored media ID")
		}
		if err = copySnapshotFile(filepath.Join(source, key), filepath.Join(s.mediaDir, key)); err != nil {
			return err
		}
	}
	return rows.Err()
}

func copySnapshotFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	return errors.Join(copyErr, closeErr)
}
