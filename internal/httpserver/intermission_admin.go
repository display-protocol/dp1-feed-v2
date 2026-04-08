package httpserver

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/display-protocol/dp1-go/playlist"
	"github.com/gin-gonic/gin"

	"github.com/display-protocol/dp1-feed-v2/internal/models"
)

const defaultIntermissionPlaylist = "ex-nihilo-full-collection"

var intermissionAdminTemplate = template.Must(template.New("intermission-admin").Parse(`
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Intermission Notes</title>
  <style>
    :root { color-scheme: light; font-family: "Helvetica Neue", Helvetica, Arial, sans-serif; }
    body { margin: 0; background: #f3efe6; color: #111; }
    main { max-width: 1080px; margin: 0 auto; padding: 24px; }
    h1, h2, h3 { margin: 0 0 12px; font-weight: 500; }
    p { margin: 0 0 12px; line-height: 1.5; }
    .panel { background: #fffdf9; border: 1px solid #d7cfbf; padding: 20px; margin-bottom: 20px; }
    .row { display: grid; gap: 16px; grid-template-columns: 1.2fr 0.8fr; }
    .stack { display: grid; gap: 12px; }
    label { display: block; font-size: 12px; font-weight: 700; letter-spacing: 0.06em; text-transform: uppercase; margin-bottom: 6px; }
    input, textarea, button { width: 100%; box-sizing: border-box; font: inherit; }
    input, textarea { padding: 10px 12px; border: 1px solid #b5ad9e; background: #fff; }
    textarea { min-height: 120px; resize: vertical; }
    button { padding: 10px 14px; border: 0; background: #111; color: #fff; cursor: pointer; }
    .muted { color: #6d665c; font-size: 14px; }
    .success { color: #1d5c2e; }
    .error { color: #8a1f1f; }
    .preview { aspect-ratio: 1 / 1; background: #000; color: #fff; display: flex; align-items: center; justify-content: center; padding: 32px; text-align: center; }
    .preview p { font-size: 24px; line-height: 1.55; max-width: 28ch; margin: 0; }
    .meta { display: flex; justify-content: space-between; gap: 12px; font-size: 13px; color: #6d665c; }
    .item { border-top: 1px solid #e3dccf; padding-top: 20px; margin-top: 20px; }
    .item:first-of-type { border-top: 0; padding-top: 0; margin-top: 0; }
    @media (max-width: 880px) { .row { grid-template-columns: 1fr; } .preview p { font-size: 20px; } }
  </style>
</head>
<body>
  <main>
    <div class="panel">
      <h1>Intermission Notes</h1>
      <p class="muted">Playlist-first prototype for Casey's exhibition playlist. Default target: <code>{{.PlaylistRef}}</code>.</p>
      {{if .Message}}<p class="success">{{.Message}}</p>{{end}}
      {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
      <form method="get" action="/admin/intermission-notes" class="stack">
        <div>
          <label for="playlist">Playlist URL, ID, or slug</label>
          <input id="playlist" name="playlist" value="{{.PlaylistRef}}">
        </div>
        <button type="submit">Load playlist</button>
      </form>
    </div>

    {{if .Loaded}}
    <div class="panel row">
      <div>
        <h2>{{.Playlist.Title}}</h2>
        <p class="muted">{{.Playlist.ID}}</p>
        <p class="muted">Slug: {{.Playlist.Slug}} · DP-1 {{.Playlist.DPVersion}}</p>
      </div>
      <div class="preview">
        <p>{{.PreviewText}}</p>
      </div>
      <div class="meta">
        <span>Preview duration: {{.PreviewDuration}}s</span>
        <span>Black/white FF1 card prototype</span>
      </div>
    </div>

    <div class="panel">
      <h2>Playlist Note</h2>
      <form method="post" action="/admin/intermission-notes" class="stack">
        <input type="hidden" name="playlist" value="{{.PlaylistRef}}">
        <input type="hidden" name="scope" value="playlist">
        <div>
          <label for="playlist-text">Note text</label>
          <textarea id="playlist-text" name="text" maxlength="500">{{.PlaylistNoteText}}</textarea>
        </div>
        <div>
          <label for="playlist-duration">Display duration</label>
          <input id="playlist-duration" name="display_duration" type="number" min="1" value="{{.PlaylistNoteDuration}}">
        </div>
        <button type="submit">{{if .HasPlaylistNote}}Replace or clear playlist note{{else}}Publish playlist note{{end}}</button>
      </form>
    </div>

    <div class="panel">
      <h2>Work Notes</h2>
      {{range .Items}}
      <div class="item">
        <h3>{{.Title}}</h3>
        <p class="muted">{{.ID}}</p>
        <form method="post" action="/admin/intermission-notes" class="stack">
          <input type="hidden" name="playlist" value="{{$.PlaylistRef}}">
          <input type="hidden" name="scope" value="item">
          <input type="hidden" name="item_id" value="{{.ID}}">
          <div>
            <label for="text-{{.ID}}">Note text</label>
            <textarea id="text-{{.ID}}" name="text" maxlength="500">{{.NoteText}}</textarea>
          </div>
          <div>
            <label for="duration-{{.ID}}">Display duration</label>
            <input id="duration-{{.ID}}" name="display_duration" type="number" min="1" value="{{.Duration}}">
          </div>
          <button type="submit">{{if .HasNote}}Replace or clear item note{{else}}Publish item note{{end}}</button>
        </form>
      </div>
      {{end}}
    </div>
    {{end}}
  </main>
</body>
</html>
`))

type intermissionAdminData struct {
	PlaylistRef          string
	Message              string
	Error                string
	Loaded               bool
	Playlist             *playlist.Playlist
	PreviewText          string
	PreviewDuration      int
	HasPlaylistNote      bool
	PlaylistNoteText     string
	PlaylistNoteDuration int
	Items                []intermissionAdminItem
}

type intermissionAdminItem struct {
	ID       string
	Title    string
	HasNote  bool
	NoteText string
	Duration int
}

func (h *Handler) IntermissionAdminPage(c *gin.Context) {
	data := intermissionAdminData{
		PlaylistRef: strings.TrimSpace(c.DefaultQuery("playlist", defaultIntermissionPlaylist)),
		Message:     strings.TrimSpace(c.Query("message")),
		Error:       strings.TrimSpace(c.Query("error")),
	}
	if data.PlaylistRef == "" {
		data.PlaylistRef = defaultIntermissionPlaylist
	}
	if data.Error == "" {
		if pl, err := h.Exec.GetPlaylist(c.Request.Context(), data.PlaylistRef); err == nil && pl != nil {
			populateIntermissionAdminData(&data, pl)
		} else if err != nil && c.Query("playlist") != "" {
			data.Error = err.Error()
		}
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Status(http.StatusOK)
	_ = intermissionAdminTemplate.Execute(c.Writer, data)
}

func (h *Handler) IntermissionAdminSubmit(c *gin.Context) {
	ref := strings.TrimSpace(c.PostForm("playlist"))
	if ref == "" {
		ref = defaultIntermissionPlaylist
	}
	if !h.authorizePublisherPlaylistWrite(c, ref) {
		return
	}
	scope := strings.TrimSpace(c.PostForm("scope"))
	itemID := strings.TrimSpace(c.PostForm("item_id"))
	text := strings.TrimSpace(c.PostForm("text"))
	duration, err := parseNoteDuration(c.PostForm("display_duration"))
	if err != nil {
		h.redirectIntermissionAdmin(c, ref, "", err.Error())
		return
	}

	current, err := h.Exec.GetPlaylist(c.Request.Context(), ref)
	if err != nil {
		h.redirectIntermissionAdmin(c, ref, "", err.Error())
		return
	}

	req := buildPlaylistReplaceRequest(current)
	switch scope {
	case "playlist":
		req.Note = makeNote(text, duration)
	case "item":
		found := false
		for i := range req.Items {
			if req.Items[i].ID == itemID {
				req.Items[i].Note = makeNote(text, duration)
				found = true
				break
			}
		}
		if !found {
			h.redirectIntermissionAdmin(c, ref, "", "playlist item not found")
			return
		}
	default:
		h.redirectIntermissionAdmin(c, ref, "", "scope must be playlist or item")
		return
	}

	if _, err := h.Exec.ReplacePlaylist(c.Request.Context(), ref, req); err != nil {
		h.redirectIntermissionAdmin(c, ref, "", err.Error())
		return
	}

	message := "Intermission note updated"
	if strings.TrimSpace(text) == "" {
		message = "Intermission note cleared"
	}
	h.redirectIntermissionAdmin(c, ref, message, "")
}

func buildPlaylistReplaceRequest(pl *playlist.Playlist) *models.PlaylistReplaceRequest {
	if pl == nil {
		return &models.PlaylistReplaceRequest{}
	}
	items := append([]playlist.PlaylistItem(nil), pl.Items...)
	curators := append(pl.Curators[:0:0], pl.Curators...)
	return &models.PlaylistReplaceRequest{
		DPVersion:    pl.DPVersion,
		Title:        pl.Title,
		Slug:         pl.Slug,
		Items:        items,
		Note:         pl.Note,
		Curators:     curators,
		Summary:      pl.Summary,
		CoverImage:   pl.CoverImage,
		Defaults:     pl.Defaults,
		DynamicQuery: pl.DynamicQuery,
	}
}

func populateIntermissionAdminData(data *intermissionAdminData, pl *playlist.Playlist) {
	if data == nil || pl == nil {
		return
	}
	data.Loaded = true
	data.Playlist = pl
	data.PreviewText = "Preview this note on the right."
	data.PreviewDuration = 20
	data.PlaylistNoteDuration = 20
	if pl.Note != nil {
		data.HasPlaylistNote = true
		data.PlaylistNoteText = pl.Note.Text
		data.PlaylistNoteDuration = noteDuration(pl.Note)
		data.PreviewText = pl.Note.Text
		data.PreviewDuration = noteDuration(pl.Note)
	}
	data.Items = make([]intermissionAdminItem, 0, len(pl.Items))
	for idx, item := range pl.Items {
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = "Item " + strconv.Itoa(idx+1)
		}
		row := intermissionAdminItem{
			ID:       item.ID,
			Title:    title,
			Duration: 20,
		}
		if item.Note != nil {
			row.HasNote = true
			row.NoteText = item.Note.Text
			row.Duration = noteDuration(item.Note)
			if !data.HasPlaylistNote && data.PreviewText == "Preview this note on the right." {
				data.PreviewText = item.Note.Text
				data.PreviewDuration = row.Duration
			}
		}
		data.Items = append(data.Items, row)
	}
}

func makeNote(text string, duration int) *playlist.Note {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	n := &playlist.Note{Text: text}
	if duration > 0 {
		n.DisplayDuration = &duration
	}
	return n
}

func noteDuration(n *playlist.Note) int {
	if n == nil || n.DisplayDuration == nil || *n.DisplayDuration <= 0 {
		return 20
	}
	return *n.DisplayDuration
}

func parseNoteDuration(v string) (int, error) {
	if strings.TrimSpace(v) == "" {
		return 20, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("display duration must be a positive integer")
	}
	return n, nil
}

func (h *Handler) redirectIntermissionAdmin(c *gin.Context, playlistRef, message, errMessage string) {
	q := url.Values{}
	q.Set("playlist", playlistRef)
	if message != "" {
		q.Set("message", message)
	}
	if errMessage != "" {
		q.Set("error", errMessage)
	}
	c.Redirect(http.StatusSeeOther, "/admin/intermission-notes?"+q.Encode())
}
