package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

func (a *app) clearGuestRuntimeState(ctx context.Context, subject string) {
	if a.redis == nil || !strings.HasPrefix(subject, "guest:") {
		return
	}
	for _, pattern := range []string{
		"quota:pending:" + subject + ":*",
		"quota:reservation:" + subject + ":*",
		"guest-imagegen:" + subject + ":*",
		"guest-filegen:" + subject + ":*",
	} {
		var cursor uint64
		for {
			keys, next, err := a.redis.Scan(ctx, cursor, pattern, 100).Result()
			if err != nil {
				break
			}
			if len(keys) > 0 {
				_ = a.redis.Del(ctx, keys...).Err()
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
	}
	if devices, err := a.store.GuestDevicesForSubject(ctx, subject); err == nil {
		for _, device := range devices {
			rateSubject := guestRateSubject(guestIdentity{Subject: subject, DeviceID: device.DeviceID})
			_ = a.redis.Del(ctx, "guest-chat:hour:"+rateSubject, "guest-chat:last:"+rateSubject).Err()
		}
	}
	_ = a.redis.Del(ctx,
		"guest-chat:hour:"+subject,
		"guest-chat:last:"+subject,
		"guest-upload:hour:"+subject,
	).Err()
}

func (a *app) adminGuestDetail(w http.ResponseWriter, r *http.Request) {
	subject := strings.TrimSpace(chi.URLParam(r, "guestSubject"))
	if !strings.HasPrefix(subject, "guest:") || len(subject) > 96 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid guest identity"})
		return
	}
	deviceID := cleanGuestHeader(r.URL.Query().Get("deviceId"), 128)
	devices, err := a.store.GuestDevicesForSubject(r.Context(), subject)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest devices unavailable"})
		return
	}
	if deviceID != "" {
		found := false
		for _, device := range devices {
			if device.DeviceID == deviceID {
				found = true
				break
			}
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "guest device not found"})
			return
		}
	}
	activity, err := a.store.RecentActivityForGuest(r.Context(), subject, deviceID, 150)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest activity unavailable"})
		return
	}
	attachments, err := a.store.GuestAttachmentsForAdmin(r.Context(), subject, deviceID, 200)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest attachments unavailable"})
		return
	}
	resets, err := a.store.GuestQuotaResetEvents(r.Context(), subject, 50)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "guest reset history unavailable"})
		return
	}
	usage, _ := a.store.GuestUsageSummary(r.Context(), subject, time.Unix(0, 0))
	p, _, _ := a.store.GuestAccessPolicy(r.Context())
	quota, _, _ := a.guestQuota(r.Context(), subject, p)
	writeJSON(w, http.StatusOK, map[string]any{
		"guestSubject":     subject,
		"selectedDeviceId": deviceID,
		"devices":          devices,
		"usage":            usage,
		"activity":         activity,
		"attachments":      attachments,
		"resets":           resets,
		"quota":            quota,
	})
}

func (a *app) adminResetGuestQuota(w http.ResponseWriter, r *http.Request) {
	subject := strings.TrimSpace(chi.URLParam(r, "guestSubject"))
	if !strings.HasPrefix(subject, "guest:") || len(subject) > 96 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid guest identity"})
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	_ = readOptionalJSON(r, &body)
	body.Note = strings.TrimSpace(body.Note)
	if body.Note == "" {
		body.Note = "Manual admin guest reset"
	}
	resetAt, err := a.store.ResetGuestQuota(r.Context(), subject, current(r).Sub, body.Note)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unable to reset guest quota"})
		return
	}
	a.clearGuestRuntimeState(r.Context(), subject)
	p, _, _ := a.store.GuestAccessPolicy(r.Context())
	decision, _, _ := a.guestQuota(r.Context(), subject, p)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "guestSubject": subject, "resetAt": resetAt, "quota": decision})
}

func readOptionalJSON(r *http.Request, dst any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(dst)
}
