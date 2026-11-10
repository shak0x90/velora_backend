package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shak0x90/velora_backend/internal/httpx"
)

func TestPhotoMessageValidation(t *testing.T) {
	att := uuid.NewString()
	bad := "not-a-uuid"
	for name, in := range map[string]SendInput{
		"image without attachment": {ClientMessageID: uuid.NewString(), Kind: "image"},
		"image with bad id":        {ClientMessageID: uuid.NewString(), Kind: "image", AttachmentID: &bad},
		"text with attachment":     {ClientMessageID: uuid.NewString(), Kind: "text", Body: "hi", AttachmentID: &att},
		"unknown kind":             {ClientMessageID: uuid.NewString(), Kind: "voice", Body: "hi"},
	} {
		if _, err := validateMessage(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A photo needs no caption.
	if _, err := validateMessage(SendInput{ClientMessageID: uuid.NewString(), Kind: "image", AttachmentID: &att}); err != nil {
		t.Errorf("captionless photo rejected: %v", err)
	}
}

func TestDirStoreRefusesEscapes(t *testing.T) {
	store := DirStore{Root: t.TempDir()}
	for _, key := range []string{"../escape-full.jpg", "/abs-full.jpg", "a/../../x.jpg"} {
		if err := store.Put(t.Context(), key, []byte("x")); err == nil {
			t.Errorf("wrote outside the root with key %q", key)
		}
	}
	if err := store.Put(t.Context(), "c/p-full.jpg", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	f, _, err := store.Open("c/p-full.jpg")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.RGBA{R: 200, A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReactions(t *testing.T) {
	f := setup(t)
	m := mustSend(t, f, f.a, f.cid, input("React to me"))

	got, err := f.s.React(f.ctx, f.b, f.cid, m.ID, "❤️")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Reactions) != 1 || got.Reactions[0] != (Reaction{Emoji: "❤️", Count: 1, Mine: true}) {
		t.Fatalf("after b reacts: %+v", got.Reactions)
	}
	// Changing replaces rather than stacks.
	if got, err = f.s.React(f.ctx, f.b, f.cid, m.ID, "😂"); err != nil || len(got.Reactions) != 1 || got.Reactions[0].Emoji != "😂" {
		t.Fatalf("change: %+v %v", got.Reactions, err)
	}
	if got, err = f.s.React(f.ctx, f.a, f.cid, m.ID, "😂"); err != nil || got.Reactions[0] != (Reaction{Emoji: "😂", Count: 2, Mine: true}) {
		t.Fatalf("both: %+v %v", got.Reactions, err)
	}

	// History carries reactions as seen by the reader.
	h, err := f.s.History(f.ctx, f.b, f.cid, -1, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	last := h.Messages[len(h.Messages)-1]
	if last.ID != m.ID || len(last.Reactions) != 1 || last.Reactions[0].Count != 2 || !last.Reactions[0].Mine {
		t.Fatalf("history: %+v", last.Reactions)
	}

	// The other person is told, so open threads refresh.
	var edited int
	if err = f.pool.QueryRow(f.ctx, `select count(*) from user_events where user_id=$1 and kind='message.edited' and payload->>'messageId'=$2`, f.a, m.ID).Scan(&edited); err != nil || edited < 2 {
		t.Fatalf("message.edited events for a: %d %v", edited, err)
	}

	if got, err = f.s.React(f.ctx, f.b, f.cid, m.ID, ""); err != nil || got.Reactions[0] != (Reaction{Emoji: "😂", Count: 1, Mine: false}) {
		t.Fatalf("clear: %+v %v", got.Reactions, err)
	}
	var problem httpx.Problem
	if _, err = f.s.React(f.ctx, f.b, f.cid, m.ID, "🍕"); !errors.As(err, &problem) || problem.Status != 400 {
		t.Fatalf("unlisted emoji: %v", err)
	}
	if _, err = f.s.React(f.ctx, f.third, f.cid, m.ID, "❤️"); !errors.As(err, &problem) || problem.Status != 404 {
		t.Fatalf("outsider reacted: %v", err)
	}
}

func TestPhotoFlow(t *testing.T) {
	f := setup(t)
	root := t.TempDir()
	f.s.WithPhotos(DirStore{Root: root})
	mux := http.NewServeMux()
	f.s.Routes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	upload := func(uid, cid string, raw []byte) (*http.Response, Attachment) {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		part, _ := mw.CreateFormFile("photo", "p.png")
		part.Write(raw)
		mw.Close()
		req, _ := http.NewRequest("POST", server.URL+"/conversations/"+cid+"/photos", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+token(uid))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var att Attachment
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		_ = json.Unmarshal(data, &att)
		return resp, att
	}
	fetch := func(uid, cid, photo, size string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", server.URL+"/conversations/"+cid+"/photos/"+photo+"?size="+size, nil)
		req.Header.Set("Authorization", "Bearer "+token(uid))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
				t.Errorf("content type %q", ct)
			}
			if cc := resp.Header.Get("Cache-Control"); !strings.HasPrefix(cc, "private") {
				t.Errorf("cache control %q", cc)
			}
		}
		return resp.StatusCode
	}

	resp, att := upload(f.a, f.cid, testPNG(t, 2400, 1200))
	if resp.StatusCode != 201 || att.ID == "" || att.Width != photoFullSide || att.Height != 800 {
		t.Fatalf("upload: %d %+v", resp.StatusCode, att)
	}
	if resp, _ = upload(f.a, f.cid, []byte("not an image")); resp.StatusCode != 400 {
		t.Fatalf("garbage upload: %d", resp.StatusCode)
	}

	// Before sending, only the uploader can see it.
	if code := fetch(f.a, f.cid, att.ID, "thumb"); code != 200 {
		t.Fatalf("uploader preview: %d", code)
	}
	if code := fetch(f.b, f.cid, att.ID, "full"); code != 404 {
		t.Fatalf("peer saw an unsent upload: %d", code)
	}

	in := SendInput{ClientMessageID: uuid.NewString(), Kind: "image", AttachmentID: &att.ID}
	m := mustSend(t, f, f.a, f.cid, in)
	if m.Kind != "image" || m.Attachment == nil || m.Attachment.ID != att.ID || m.Body != "" {
		t.Fatalf("sent: %+v", m)
	}
	// A retry with the same clientMessageId returns the same message.
	if again := mustSend(t, f, f.a, f.cid, in); again.ID != m.ID {
		t.Fatal("retry made a second photo message")
	}
	// The attachment cannot be reused by a different send.
	if _, err := f.s.Send(f.ctx, f.a, f.cid, SendInput{ClientMessageID: uuid.NewString(), Kind: "image", AttachmentID: &att.ID}); err == nil {
		t.Fatal("one upload attached to two messages")
	}
	if code := fetch(f.b, f.cid, att.ID, "full"); code != 200 {
		t.Fatalf("recipient could not open the photo: %d", code)
	}
	if code := fetch(f.third, f.cid, att.ID, "full"); code != 404 {
		t.Fatalf("outsider opened the photo: %d", code)
	}
	if code := fetch(f.b, f.cid, att.ID, "huge"); code != 400 {
		t.Fatalf("bad size: %d", code)
	}

	snap, err := f.s.Bootstrap(f.ctx, f.b, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range snap.Conversations {
		if c.ID == f.cid && c.Preview != "📷 Photo" {
			t.Errorf("inbox preview %q", c.Preview)
		}
	}

	// A message request cannot open with a photo.
	rid, err := f.s.Open(f.ctx, f.third, f.a, "request", input("Hi there"))
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ = upload(f.third, rid, testPNG(t, 50, 50)); resp.StatusCode != 403 {
		t.Fatalf("photo into a pending request: %d", resp.StatusCode)
	}

	// A block cuts off the photo along with the text.
	if _, err = f.pool.Exec(f.ctx, `insert into blocks(user_id,blocked_id) values($1,$2)`, f.b, f.a); err != nil {
		t.Fatal(err)
	}
	if code := fetch(f.b, f.cid, att.ID, "full"); code != 404 {
		t.Fatalf("photo still served after a block: %d", code)
	}
	if _, err = f.pool.Exec(f.ctx, `delete from blocks where user_id=$1 and blocked_id=$2`, f.b, f.a); err != nil {
		t.Fatal(err)
	}

	// Unsent uploads are swept, files included.
	_, stale := upload(f.a, f.cid, testPNG(t, 40, 40))
	if _, err = f.pool.Exec(f.ctx, `update message_attachments set created_at=now()-interval '2 days' where id=$1`, stale.ID); err != nil {
		t.Fatal(err)
	}
	f.s.pruneUnsentPhotos(f.ctx)
	if _, err = os.Stat(filepath.Join(root, f.cid, stale.ID+"-full.jpg")); !os.IsNotExist(err) {
		t.Fatalf("stale upload file kept: %v", err)
	}
	if _, err = os.Stat(filepath.Join(root, f.cid, att.ID+"-full.jpg")); err != nil {
		t.Fatalf("a sent photo was pruned: %v", err)
	}
}
