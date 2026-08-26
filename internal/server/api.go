package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/deploy103/qr-url-web/internal/qr"
	"github.com/deploy103/qr-url-web/internal/store"
	"github.com/deploy103/qr-url-web/internal/validate"
)

// decodeJSON reads a JSON request body, rejecting unknown fields so a typo in
// the client never silently does nothing.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "요청 본문이 너무 큽니다.")
			return false
		}
		writeJSONError(w, http.StatusBadRequest, "요청 형식이 올바르지 않습니다.")
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSONError(w, http.StatusBadRequest, "요청 본문이 올바르지 않습니다.")
		return false
	}
	return true
}

// apiError maps an internal error onto an HTTP response.
func (s *Server) apiError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "대상을 찾을 수 없습니다.")
	case errors.Is(err, store.ErrCodeTaken):
		writeJSONError(w, http.StatusConflict, "이미 사용 중인 단축 코드입니다.")
	case errors.Is(err, store.ErrNoCodesLeft):
		writeJSONError(w, http.StatusConflict, "001~999 번호를 모두 사용했습니다. 기존 항목을 삭제해 주세요.")
	case errors.Is(err, store.ErrInvalidInput):
		writeJSONError(w, http.StatusBadRequest, "요청 값이 올바르지 않습니다.")
	case isUserError(err):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, qr.ErrTooLong), errors.Is(err, qr.ErrEmptyPayload):
		writeJSONError(w, http.StatusBadRequest, qrEncodeMessage(err))
	default:
		s.log.Error("server: "+op, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "서버에서 오류가 발생했습니다.")
	}
}

func badRequest(w http.ResponseWriter, err error) {
	writeJSONError(w, http.StatusBadRequest, err.Error())
}

// ---------------------------------------------------------------- overview

type overviewResponse struct {
	Dashboard           store.Dashboard     `json:"dashboard"`
	ExpiringShortURLs   []shortURLView      `json:"expiringShortUrls"`
	ExpiringQRCodes     []qrView            `json:"expiringQrCodes"`
	RecentShortURLs     []shortURLView      `json:"recentShortUrls"`
	RecentQRCodes       []qrView            `json:"recentQrCodes"`
	Cleanup             store.CleanupStatus `json:"cleanup"`
	ServerTime          time.Time           `json:"serverTime"`
	Timezone            string              `json:"timezone"`
	ExpiringWindowHours int                 `json:"expiringWindowHours"`
	BaseURL             string              `json:"baseUrl"`
	Version             string              `json:"version"`
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	if s.cleaner != nil {
		s.cleaner.Touch(r.Context())
	}
	ctx := r.Context()
	now := s.now()

	dash, err := s.db.Stats(ctx, now)
	if err != nil {
		s.apiError(w, "dashboard", err)
		return
	}
	expiringURLs, err := s.db.ExpiringShortURLs(ctx, now, 10)
	if err != nil {
		s.apiError(w, "expiring short urls", err)
		return
	}
	expiringQRs, err := s.db.ExpiringQRCodes(ctx, now, 10)
	if err != nil {
		s.apiError(w, "expiring qr codes", err)
		return
	}
	recentURLs, err := s.db.RecentShortURLs(ctx, now, 8)
	if err != nil {
		s.apiError(w, "recent short urls", err)
		return
	}
	recentQRs, err := s.db.RecentQRCodes(ctx, now, 8)
	if err != nil {
		s.apiError(w, "recent qr codes", err)
		return
	}
	cleanupStatus, err := s.db.CleanupStatus(ctx)
	if err != nil {
		s.apiError(w, "cleanup status", err)
		return
	}

	writeJSON(w, http.StatusOK, overviewResponse{
		Dashboard:           dash,
		ExpiringShortURLs:   s.shortURLViews(expiringURLs, now),
		ExpiringQRCodes:     s.qrViews(expiringQRs, now),
		RecentShortURLs:     s.shortURLViews(recentURLs, now),
		RecentQRCodes:       s.qrViews(recentQRs, now),
		Cleanup:             cleanupStatus,
		ServerTime:          now,
		Timezone:            s.cfg.Location.String(),
		ExpiringWindowHours: int(store.ExpiringSoonWindow() / time.Hour),
		BaseURL:             s.cfg.BaseURL,
		Version:             BuildVersion,
	})
}

func (s *Server) handleRunCleanup(w http.ResponseWriter, r *http.Request) {
	if s.cleaner == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "정리 작업을 사용할 수 없습니다.")
		return
	}
	res, err := s.cleaner.RunOnce(r.Context())
	if err != nil {
		s.apiError(w, "cleanup", err)
		return
	}
	if !res.Skipped {
		s.audit(r, store.AuditCleanup, "manual",
			fmt.Sprintf("%d short URLs, %d QR codes, %d files", res.ShortURLs, res.QRCodes, res.Files+res.Orphans))
	}
	writeJSON(w, http.StatusOK, res)
}

// -------------------------------------------------------------- short URLs

// listPageSize bounds a single listing response. There can never be more than
// 999 short URLs, but the QR library is unbounded, so the count is always
// reported alongside the page and the console says when it was cut short.
const listPageSize = 500

func (s *Server) handleListShortURLs(w http.ResponseWriter, r *http.Request) {
	if s.cleaner != nil {
		s.cleaner.Touch(r.Context())
	}
	q := r.URL.Query()
	now := s.now()
	offset := pageOffset(q.Get("offset"))
	filter := store.ShortURLFilter{
		Query:  q.Get("q"),
		Status: q.Get("status"),
		Sort:   q.Get("sort"),
		Order:  q.Get("order"),
		Limit:  listPageSize,
		Offset: offset,
	}
	links, err := s.db.ListShortURLs(r.Context(), filter, now)
	if err != nil {
		s.apiError(w, "list short urls", err)
		return
	}
	total, err := s.db.CountShortURLs(r.Context(), filter, now)
	if err != nil {
		s.apiError(w, "count short urls", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":     s.shortURLViews(links, now),
		"total":     total,
		"offset":    offset,
		"truncated": int64(offset+len(links)) < total,
	})
}

type createShortURLRequest struct {
	Code          string `json:"code"`
	TargetURL     string `json:"targetUrl"`
	Note          string `json:"note"`
	Enabled       *bool  `json:"enabled"`
	ExpiryPreset  string `json:"expiryPreset"`
	ExpiryCustom  string `json:"expiryCustom"`
	CreateQR      bool   `json:"createQr"`
	QRLabel       string `json:"qrLabel"`
	QRECLevel     string `json:"qrEcLevel"`
	QRSize        int    `json:"qrSize"`
	QRExpiryUsing string `json:"qrExpiryUsing"` // "link" mirrors the link expiry
	QRExpiryPre   string `json:"qrExpiryPreset"`
	QRExpiryCust  string `json:"qrExpiryCustom"`
}

func (s *Server) handleCreateShortURL(w http.ResponseWriter, r *http.Request) {
	var req createShortURLRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	now := s.now()

	in, err := s.shortURLInput(req, now)
	if err != nil {
		badRequest(w, err)
		return
	}
	var qrLevel qr.Level
	var qrSize int
	if req.CreateQR {
		if qrLevel, qrSize, err = renderOptions(req.QRECLevel, req.QRSize); err != nil {
			badRequest(w, err)
			return
		}
	}
	link, err := s.db.CreateShortURL(r.Context(), in, now)
	if err != nil {
		s.apiError(w, "create short url", err)
		return
	}

	s.audit(r, store.AuditShortCreate, "/"+link.Code, link.TargetURL)

	resp := map[string]any{"item": s.shortURLView(link, now)}
	if req.CreateQR {
		expiry := link.ExpiresAt
		if req.QRExpiryUsing != "link" {
			expiry, err = validate.Expiry(req.QRExpiryPre, req.QRExpiryCust, now, s.cfg.Location)
			if err != nil {
				// The link itself was created; report the QR problem only.
				resp["qrError"] = err.Error()
				writeJSON(w, http.StatusCreated, resp)
				return
			}
		}
		qrRecord, err := s.createQRRecord(r.Context(), qrCreation{
			Kind:      store.KindShort,
			Payload:   s.cfg.ShortURL(link.Code),
			Label:     req.QRLabel,
			ShortCode: &link.Code,
			Level:     qrLevel,
			Size:      qrSize,
			ExpiresAt: expiry,
		}, now)
		if err != nil {
			resp["qrError"] = "QR을 만들지 못했습니다."
			s.log.Error("server: create qr for new short url", "error", err)
		} else {
			s.audit(r, store.AuditQRCreate, qrRecord.ID, "short "+qrRecord.Payload)
			resp["qr"] = s.qrView(qrRecord, now)
		}
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) shortURLInput(req createShortURLRequest, now time.Time) (store.ShortURLInput, error) {
	var in store.ShortURLInput
	target, err := validate.TargetURL(req.TargetURL)
	if err != nil {
		return in, err
	}
	note, err := validate.Note(req.Note)
	if err != nil {
		return in, err
	}
	code := ""
	if req.Code != "" {
		if code, err = validate.ShortCode(req.Code); err != nil {
			return in, err
		}
	}
	expires, err := validate.Expiry(req.ExpiryPreset, req.ExpiryCustom, now, s.cfg.Location)
	if err != nil {
		return in, err
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return store.ShortURLInput{
		Code:      code,
		TargetURL: target,
		Note:      note,
		Enabled:   enabled,
		ExpiresAt: expires,
	}, nil
}

type updateShortURLRequest struct {
	Code         *string `json:"code"`
	TargetURL    *string `json:"targetUrl"`
	Note         *string `json:"note"`
	Enabled      *bool   `json:"enabled"`
	ExpiryPreset *string `json:"expiryPreset"`
	ExpiryCustom string  `json:"expiryCustom"`
	ResetClicks  bool    `json:"resetClicks"`
}

func (s *Server) handleUpdateShortURL(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	var req updateShortURLRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	now := s.now()

	up := store.ShortURLUpdate{Enabled: req.Enabled, ResetClick: req.ResetClicks}
	if req.Code != nil {
		code, err := validate.ShortCode(*req.Code)
		if err != nil {
			badRequest(w, err)
			return
		}
		up.Code = &code
	}
	if req.TargetURL != nil {
		target, err := validate.TargetURL(*req.TargetURL)
		if err != nil {
			badRequest(w, err)
			return
		}
		up.TargetURL = &target
	}
	if req.Note != nil {
		note, err := validate.Note(*req.Note)
		if err != nil {
			badRequest(w, err)
			return
		}
		up.Note = &note
	}
	if req.ExpiryPreset != nil {
		expires, err := validate.Expiry(*req.ExpiryPreset, req.ExpiryCustom, now, s.cfg.Location)
		if err != nil {
			badRequest(w, err)
			return
		}
		up.ExpiresAt = &expires
	}

	previous, err := s.db.ShortURLByID(r.Context(), id)
	if err != nil {
		s.apiError(w, "load short url", err)
		return
	}
	link, err := s.db.UpdateShortURL(r.Context(), id, up, now)
	if err != nil {
		s.apiError(w, "update short url", err)
		return
	}
	if up.Code != nil && *up.Code != previous.Code {
		// The payload of the linked QR codes changed, so the images have to be
		// rendered again.
		s.rerenderLinkedQRCodes(r.Context(), link.Code, now)
	}
	s.audit(r, store.AuditShortUpdate, "/"+link.Code, describeShortURLUpdate(previous, link))
	writeJSON(w, http.StatusOK, map[string]any{"item": s.shortURLView(link, now)})
}

func (s *Server) rerenderLinkedQRCodes(ctx context.Context, code string, now time.Time) {
	records, err := s.db.QRCodesByShortCode(ctx, code)
	if err != nil {
		s.log.Error("server: load linked qr codes", "error", err)
		return
	}
	for _, rec := range records {
		level, err := qr.ParseLevel(rec.ECLevel)
		if err != nil {
			continue
		}
		rendered, err := s.files.Render(rec.ID, rec.Payload, level, rec.Size)
		if err != nil {
			s.log.Error("server: re-render qr", "id", rec.ID, "error", err)
			continue
		}
		version := rendered.Version
		if _, err := s.db.UpdateQRCode(ctx, rec.ID, store.QRCodeUpdate{Version: &version}, now); err != nil {
			s.log.Error("server: update re-rendered qr", "id", rec.ID, "error", err)
		}
	}
}

func (s *Server) handleDeleteShortURL(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	link, files, err := s.db.DeleteShortURL(r.Context(), id)
	if err != nil {
		s.apiError(w, "delete short url", err)
		return
	}
	if err := s.files.Remove(files...); err != nil {
		s.log.Error("server: remove qr files", "error", err)
	}
	s.audit(r, store.AuditShortDelete, "/"+link.Code,
		fmt.Sprintf("target %s, %d linked QR files removed", link.TargetURL, len(files)))
	s.log.Info("short url deleted", "code", link.Code, "linkedQRFiles", len(files))
	writeJSON(w, http.StatusOK, map[string]any{"deleted": link.Code, "removedFiles": len(files)})
}

type extendRequest struct {
	Duration string `json:"duration"`
}

func (s *Server) handleExtendShortURL(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	var req extendRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	d, err := validate.ParseExtendDuration(req.Duration)
	if err != nil {
		badRequest(w, err)
		return
	}
	now := s.now()
	link, err := s.db.ShortURLByID(r.Context(), id)
	if err != nil {
		s.apiError(w, "load short url", err)
		return
	}
	expires, err := validate.Extend(link.ExpiresAt, d, now)
	if err != nil {
		badRequest(w, err)
		return
	}
	updated, err := s.db.UpdateShortURL(r.Context(), id, store.ShortURLUpdate{ExpiresAt: &expires}, now)
	if err != nil {
		s.apiError(w, "extend short url", err)
		return
	}
	s.audit(r, store.AuditShortExtend, "/"+updated.Code, "by "+req.Duration+" to "+expires.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, map[string]any{"item": s.shortURLView(updated, now)})
}

// --------------------------------------------------------------- QR codes

func (s *Server) handleListQRCodes(w http.ResponseWriter, r *http.Request) {
	if s.cleaner != nil {
		s.cleaner.Touch(r.Context())
	}
	q := r.URL.Query()
	now := s.now()
	offset := pageOffset(q.Get("offset"))
	filter := store.QRFilter{
		Query:  q.Get("q"),
		Kind:   q.Get("kind"),
		Status: q.Get("status"),
		Sort:   q.Get("sort"),
		Order:  q.Get("order"),
		Limit:  listPageSize,
		Offset: offset,
	}
	records, err := s.db.ListQRCodes(r.Context(), filter, now)
	if err != nil {
		s.apiError(w, "list qr codes", err)
		return
	}
	total, err := s.db.CountQRCodes(r.Context(), filter, now)
	if err != nil {
		s.apiError(w, "count qr codes", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":     s.qrViews(records, now),
		"total":     total,
		"offset":    offset,
		"truncated": int64(offset+len(records)) < total,
	})
}

type createQRRequest struct {
	Kind              string `json:"kind"`
	Payload           string `json:"payload"`
	Label             string `json:"label"`
	ECLevel           string `json:"ecLevel"`
	Size              int    `json:"size"`
	ExpiryPreset      string `json:"expiryPreset"`
	ExpiryCustom      string `json:"expiryCustom"`
	ShortCode         string `json:"shortCode"`
	TargetURL         string `json:"targetUrl"`
	NewCode           string `json:"newCode"`
	Note              string `json:"note"`
	ShortExpiryUsing  string `json:"shortExpiryUsing"` // "qr" mirrors the QR expiry
	ShortExpiryPreset string `json:"shortExpiryPreset"`
	ShortExpiryCustom string `json:"shortExpiryCustom"`
}

func (s *Server) handleCreateQRCode(w http.ResponseWriter, r *http.Request) {
	var req createQRRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	now := s.now()

	kind, err := validate.QRKind(req.Kind)
	if err != nil {
		badRequest(w, err)
		return
	}
	label, err := validate.Label(req.Label)
	if err != nil {
		badRequest(w, err)
		return
	}
	expires, err := validate.Expiry(req.ExpiryPreset, req.ExpiryCustom, now, s.cfg.Location)
	if err != nil {
		badRequest(w, err)
		return
	}
	level, size, err := renderOptions(req.ECLevel, req.Size)
	if err != nil {
		badRequest(w, err)
		return
	}

	creation := qrCreation{Kind: kind, Label: label, Level: level, Size: size, ExpiresAt: expires}
	var linkedShort *store.ShortURL
	linkWasCreated := false

	switch kind {
	case store.KindURL:
		target, err := validate.TargetURL(req.Payload)
		if err != nil {
			badRequest(w, err)
			return
		}
		creation.Payload = target
	case store.KindText:
		payload, err := validate.Payload(req.Payload)
		if err != nil {
			badRequest(w, err)
			return
		}
		creation.Payload = payload
	case store.KindShort:
		link, created, err := s.resolveShortURLForQR(r.Context(), req, expires, now)
		if err != nil {
			if isUserError(err) {
				badRequest(w, err)
			} else {
				s.apiError(w, "prepare short url for qr", err)
			}
			return
		}
		creation.Payload = s.cfg.ShortURL(link.Code)
		creation.ShortCode = &link.Code
		linkedShort = &link
		linkWasCreated = created
	}

	record, err := s.createQRRecord(r.Context(), creation, now)
	if err != nil {
		// A short URL created a moment ago just for this QR would be left
		// behind, so roll it back rather than leaking a code. An existing one
		// that the administrator picked is of course left alone.
		if linkWasCreated && linkedShort != nil {
			if _, files, delErr := s.db.DeleteShortURL(r.Context(), linkedShort.ID); delErr != nil {
				s.log.Error("server: roll back short url", "code", linkedShort.Code, "error", delErr)
			} else if rmErr := s.files.Remove(files...); rmErr != nil {
				s.log.Error("server: roll back short url files", "error", rmErr)
			}
		}
		s.apiError(w, "create qr code", err)
		return
	}
	s.audit(r, store.AuditQRCreate, record.ID, record.Kind+" "+record.Payload)

	resp := map[string]any{"item": s.qrView(record, now)}
	if linkedShort != nil {
		if linkWasCreated {
			s.audit(r, store.AuditShortCreate, "/"+linkedShort.Code, linkedShort.TargetURL)
		}
		resp["shortUrl"] = s.shortURLView(*linkedShort, now)
	}
	writeJSON(w, http.StatusCreated, resp)
}

// resolveShortURLForQR either loads the requested existing code or creates a
// new short URL for the QR being generated.
// The boolean result reports whether the short URL was created by this call,
// which decides whether it may be rolled back if the QR fails to render.
func (s *Server) resolveShortURLForQR(ctx context.Context, req createQRRequest, qrExpiry *time.Time, now time.Time) (store.ShortURL, bool, error) {
	if req.ShortCode != "" {
		code, err := validate.ShortCode(req.ShortCode)
		if err != nil {
			return store.ShortURL{}, false, err
		}
		link, err := s.db.ShortURLByCode(ctx, code)
		return link, false, err
	}
	if req.TargetURL == "" {
		return store.ShortURL{}, false, errUser("연결할 단축 URL을 선택하거나 새 대상 주소를 입력해 주세요")
	}
	linkExpiry := qrExpiry
	if req.ShortExpiryUsing != "qr" {
		var err error
		linkExpiry, err = validate.Expiry(req.ShortExpiryPreset, req.ShortExpiryCustom, now, s.cfg.Location)
		if err != nil {
			return store.ShortURL{}, false, err
		}
	}
	in, err := s.shortURLInput(createShortURLRequest{
		Code:      req.NewCode,
		TargetURL: req.TargetURL,
		Note:      req.Note,
	}, now)
	if err != nil {
		return store.ShortURL{}, false, err
	}
	in.ExpiresAt = linkExpiry
	link, err := s.db.CreateShortURL(ctx, in, now)
	return link, err == nil, err
}

type qrCreation struct {
	Kind      string
	Payload   string
	Label     string
	ShortCode *string
	Level     qr.Level
	Size      int
	ExpiresAt *time.Time
}

// renderOptions validates the error correction level and pixel size a request
// asked for. A zero size selects the default.
func renderOptions(ecLevel string, size int) (qr.Level, int, error) {
	level, err := validate.ECLevel(ecLevel)
	if err != nil {
		return 0, 0, err
	}
	if size == 0 {
		return level, defaultQRSize, nil
	}
	checked, err := validate.Size(strconv.Itoa(size), defaultQRSize)
	if err != nil {
		return 0, 0, err
	}
	return level, checked, nil
}

// defaultQRSize is the pixel size used when a request does not ask for one.
const defaultQRSize = 512

// createQRRecord renders the images first and stores the record afterwards, so
// a failed render never leaves a dangling row.
func (s *Server) createQRRecord(ctx context.Context, in qrCreation, now time.Time) (store.QRCode, error) {
	level, size := in.Level, in.Size
	if size == 0 {
		size = defaultQRSize
	}

	id, err := store.NewQRID()
	if err != nil {
		return store.QRCode{}, err
	}
	rendered, err := s.files.Render(id, in.Payload, level, size)
	if err != nil {
		return store.QRCode{}, err
	}

	record, err := s.db.CreateQRCode(ctx, store.QRCodeInput{
		ID:        id,
		Kind:      in.Kind,
		Payload:   in.Payload,
		Label:     in.Label,
		ShortCode: in.ShortCode,
		ECLevel:   level.String(),
		Size:      size,
		Version:   rendered.Version,
		PNGPath:   rendered.PNGName,
		SVGPath:   rendered.SVGName,
		ExpiresAt: in.ExpiresAt,
	}, now)
	if err != nil {
		if rmErr := s.files.Remove(rendered.PNGName, rendered.SVGName); rmErr != nil {
			s.log.Error("server: roll back qr files", "error", rmErr)
		}
		return store.QRCode{}, err
	}
	return record, nil
}

type createQRForShortURLRequest struct {
	Label        string `json:"label"`
	ECLevel      string `json:"ecLevel"`
	Size         int    `json:"size"`
	ExpiryUsing  string `json:"expiryUsing"` // "link" mirrors the link expiry
	ExpiryPreset string `json:"expiryPreset"`
	ExpiryCustom string `json:"expiryCustom"`
}

func (s *Server) handleCreateQRForShortURL(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	var req createQRForShortURLRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	now := s.now()
	link, err := s.db.ShortURLByID(r.Context(), id)
	if err != nil {
		s.apiError(w, "load short url", err)
		return
	}
	expires := link.ExpiresAt
	if req.ExpiryUsing != "link" {
		expires, err = validate.Expiry(req.ExpiryPreset, req.ExpiryCustom, now, s.cfg.Location)
		if err != nil {
			badRequest(w, err)
			return
		}
	}
	label, err := validate.Label(req.Label)
	if err != nil {
		badRequest(w, err)
		return
	}
	level, size, err := renderOptions(req.ECLevel, req.Size)
	if err != nil {
		badRequest(w, err)
		return
	}
	record, err := s.createQRRecord(r.Context(), qrCreation{
		Kind:      store.KindShort,
		Payload:   s.cfg.ShortURL(link.Code),
		Label:     label,
		ShortCode: &link.Code,
		Level:     level,
		Size:      size,
		ExpiresAt: expires,
	}, now)
	if err != nil {
		s.apiError(w, "create qr code", err)
		return
	}
	s.audit(r, store.AuditQRCreate, record.ID, "short "+record.Payload)
	writeJSON(w, http.StatusCreated, map[string]any{"item": s.qrView(record, now)})
}

type updateQRRequest struct {
	Label        *string `json:"label"`
	ExpiryPreset *string `json:"expiryPreset"`
	ExpiryCustom string  `json:"expiryCustom"`
}

func (s *Server) handleUpdateQRCode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req updateQRRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	now := s.now()
	var up store.QRCodeUpdate
	if req.Label != nil {
		label, err := validate.Label(*req.Label)
		if err != nil {
			badRequest(w, err)
			return
		}
		up.Label = &label
	}
	if req.ExpiryPreset != nil {
		expires, err := validate.Expiry(*req.ExpiryPreset, req.ExpiryCustom, now, s.cfg.Location)
		if err != nil {
			badRequest(w, err)
			return
		}
		up.ExpiresAt = &expires
	}
	record, err := s.db.UpdateQRCode(r.Context(), id, up, now)
	if err != nil {
		s.apiError(w, "update qr code", err)
		return
	}
	s.audit(r, store.AuditQRUpdate, record.ID, describeExpiry(record.ExpiresAt))
	writeJSON(w, http.StatusOK, map[string]any{"item": s.qrView(record, now)})
}

func (s *Server) handleExtendQRCode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req extendRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	d, err := validate.ParseExtendDuration(req.Duration)
	if err != nil {
		badRequest(w, err)
		return
	}
	now := s.now()
	record, err := s.db.QRCodeByID(r.Context(), id)
	if err != nil {
		s.apiError(w, "load qr code", err)
		return
	}
	expires, err := validate.Extend(record.ExpiresAt, d, now)
	if err != nil {
		badRequest(w, err)
		return
	}
	updated, err := s.db.UpdateQRCode(r.Context(), id, store.QRCodeUpdate{ExpiresAt: &expires}, now)
	if err != nil {
		s.apiError(w, "extend qr code", err)
		return
	}
	s.audit(r, store.AuditQRExtend, updated.ID, "by "+req.Duration+" to "+expires.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, map[string]any{"item": s.qrView(updated, now)})
}

type regenerateQRRequest struct {
	ECLevel string `json:"ecLevel"`
	Size    int    `json:"size"`
}

func (s *Server) handleRegenerateQRCode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req regenerateQRRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	now := s.now()
	record, err := s.db.QRCodeByID(r.Context(), id)
	if err != nil {
		s.apiError(w, "load qr code", err)
		return
	}
	levelText := req.ECLevel
	if levelText == "" {
		levelText = record.ECLevel
	}
	level, err := validate.ECLevel(levelText)
	if err != nil {
		badRequest(w, err)
		return
	}
	size := record.Size
	if req.Size != 0 {
		if size, err = validate.Size(strconv.Itoa(req.Size), record.Size); err != nil {
			badRequest(w, err)
			return
		}
	}

	rendered, err := s.files.Render(record.ID, record.Payload, level, size)
	if err != nil {
		s.apiError(w, "render qr code", err)
		return
	}
	levelName := level.String()
	updated, err := s.db.UpdateQRCode(r.Context(), id, store.QRCodeUpdate{
		ECLevel: &levelName,
		Size:    &size,
		Version: &rendered.Version,
		PNGPath: &rendered.PNGName,
		SVGPath: &rendered.SVGName,
	}, now)
	if err != nil {
		s.apiError(w, "update qr code", err)
		return
	}
	s.audit(r, store.AuditQRRegenerate, updated.ID, fmt.Sprintf("level %s, %dpx", levelName, size))
	writeJSON(w, http.StatusOK, map[string]any{"item": s.qrView(updated, now)})
}

func (s *Server) handleDeleteQRCode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	record, files, err := s.db.DeleteQRCode(r.Context(), id)
	if err != nil {
		s.apiError(w, "delete qr code", err)
		return
	}
	if err := s.files.Remove(files...); err != nil {
		s.log.Error("server: remove qr files", "error", err)
	}
	s.audit(r, store.AuditQRDelete, record.ID, record.Kind+" "+record.Payload)
	s.log.Info("qr code deleted", "id", record.ID, "kind", record.Kind, "files", len(files))
	writeJSON(w, http.StatusOK, map[string]any{"deleted": record.ID, "removedFiles": len(files)})
}

// ------------------------------------------------------------------ helpers

// pageOffset parses a listing offset, treating anything unusable as the first
// page rather than failing the request.
func pageOffset(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func pathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "식별자가 올바르지 않습니다.")
		return 0, false
	}
	return id, true
}

// userError marks a validation failure that is safe to show verbatim.
type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

func errUser(format string, args ...any) error {
	return userError{msg: fmt.Sprintf(format, args...)}
}

func isUserError(err error) bool {
	var ue userError
	if errors.As(err, &ue) {
		return true
	}
	// The validate package returns plain sentinel errors that are already
	// written for the administrator.
	return errors.Is(err, validate.ErrCodeFormat) ||
		errors.Is(err, validate.ErrEmptyCode) ||
		errors.Is(err, validate.ErrEmptyURL) ||
		errors.Is(err, validate.ErrURLScheme) ||
		errors.Is(err, validate.ErrURLHost) ||
		errors.Is(err, validate.ErrURLCredentials) ||
		errors.Is(err, validate.ErrURLControlChar) ||
		errors.Is(err, validate.ErrURLTooLong) ||
		errors.Is(err, validate.ErrExpiryPast) ||
		errors.Is(err, validate.ErrExpiryTooFar) ||
		errors.Is(err, validate.ErrExpiryFormat) ||
		errors.Is(err, validate.ErrExpiryPreset) ||
		errors.Is(err, validate.ErrNoteTooLong) ||
		errors.Is(err, validate.ErrLabelTooLong) ||
		errors.Is(err, validate.ErrLevelOutOfRange) ||
		errors.Is(err, validate.ErrSizeOutOfRange) ||
		errors.Is(err, validate.ErrEmptyPayload) ||
		errors.Is(err, validate.ErrPayloadTooLong) ||
		errors.Is(err, validate.ErrPayloadNotUTF8) ||
		errors.Is(err, validate.ErrUnknownQRKind)
}

// describeShortURLUpdate renders a short, human readable diff for the audit
// log without ever repeating a secret.
func describeShortURLUpdate(before, after store.ShortURL) string {
	var parts []string
	if before.Code != after.Code {
		parts = append(parts, "code "+before.Code+" to "+after.Code)
	}
	if before.TargetURL != after.TargetURL {
		parts = append(parts, "target to "+after.TargetURL)
	}
	if before.Note != after.Note {
		parts = append(parts, "note changed")
	}
	if before.Enabled != after.Enabled {
		if after.Enabled {
			parts = append(parts, "enabled")
		} else {
			parts = append(parts, "disabled")
		}
	}
	if !sameExpiry(before.ExpiresAt, after.ExpiresAt) {
		parts = append(parts, "expiry "+describeExpiry(after.ExpiresAt))
	}
	if len(parts) == 0 {
		return "no effective change"
	}
	return strings.Join(parts, ", ")
}

func sameExpiry(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(*b)
	}
}

func describeExpiry(t *time.Time) string {
	if t == nil {
		return "no expiry"
	}
	return "expires " + t.Format(time.RFC3339)
}
