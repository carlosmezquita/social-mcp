package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func (p *platform) instagram(ctx context.Context, t Target, a Account, m Media, token string) (Result, error) {
	form := url.Values{"caption": {bodyText(t)}}
	if m.MIME == "video/mp4" {
		form.Set("media_type", "REELS")
		form.Set("video_url", p.app.mediaURL(m.ID))
	} else {
		form.Set("image_url", p.app.mediaURL(m.ID))
	}
	var creation struct{ ID string }
	_, err := exchange(ctx, p.http, http.MethodPost, p.base+"/"+a.RemoteID+"/media", p.headers(token), "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), -1, &creation, false)
	if err != nil {
		return Result{}, err
	}
	if !digitID.MatchString(creation.ID) {
		return Result{}, unknown(false)
	}
	for {
		var state struct {
			Code string `json:"status_code"`
		}
		if _, err = p.json(ctx, http.MethodGet, "/"+creation.ID+"?fields=status_code", token, nil, &state, false); err != nil {
			return Result{}, err
		}
		if state.Code == "FINISHED" {
			break
		}
		if state.Code != "IN_PROGRESS" {
			return Result{}, fail("permanent", "Instagram media container failed or expired")
		}
		if err = pause(ctx, 5*time.Second); err != nil {
			return Result{}, err
		}
	}
	form = url.Values{"creation_id": {creation.ID}}
	var post struct{ ID string }
	_, err = exchange(ctx, p.http, http.MethodPost, p.base+"/"+a.RemoteID+"/media_publish", p.headers(token), "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), -1, &post, true)
	if f, ok := err.(*Failure); ok && f.Kind == "ambiguous" {
		// A published status proves the container was committed even if the media ID was lost.
		var state struct {
			Code string `json:"status_code"`
		}
		if _, check := p.json(ctx, http.MethodGet, "/"+creation.ID+"?fields=status_code", token, nil, &state, false); check == nil && state.Code == "PUBLISHED" {
			return Result{}, fail("ambiguous", "Instagram confirmed the container was published; verify post ID before any resend")
		}
	}
	if !digitID.MatchString(post.ID) && err == nil {
		err = unknown(true)
	}
	return confirmed(post.ID, err)
}

type xMediaResponse struct {
	Data struct {
		ID         string `json:"id"`
		Processing *struct {
			State string `json:"state"`
			Wait  int    `json:"check_after_secs"`
		} `json:"processing_info"`
	} `json:"data"`
}

func (p *platform) x(ctx context.Context, t Target, m Media, token string) (Result, error) {
	payload := fields{"text": bodyText(t)}
	if m.ID != "" {
		mediaID, err := p.uploadX(ctx, m, token)
		if err != nil {
			return Result{}, err
		}
		payload["media"] = fields{"media_ids": []string{mediaID}}
		if payload["text"] == "" {
			delete(payload, "text")
		}
	}
	var reply struct{ Data struct{ ID string } }
	_, err := p.json(ctx, http.MethodPost, "/2/tweets", token, payload, &reply, true)
	if !digitID.MatchString(reply.Data.ID) && err == nil {
		err = unknown(true)
	}
	result, err := confirmed(reply.Data.ID, err)
	if err == nil {
		result.URL = "https://x.com/i/web/status/" + result.ID
	}
	return result, err
}
func (p *platform) uploadX(ctx context.Context, m Media, token string) (string, error) {
	f, err := os.Open(m.Path)
	if err != nil {
		return "", fail("permanent", "cannot open stored media")
	}
	defer f.Close()
	category := "tweet_image"
	if m.MIME == "video/mp4" {
		category = "tweet_video"
	}
	const root = "/2/media/upload"
	var response xMediaResponse
	_, err = p.json(ctx, http.MethodPost, root+"/initialize", token, fields{"media_type": m.MIME, "total_bytes": m.Size, "media_category": category}, &response, false)
	if err != nil {
		return "", err
	}
	key := response.Data.ID
	if !digitID.MatchString(key) {
		return "", unknown(false)
	}
	for offset, index := int64(0), 0; offset < m.Size; index++ {
		count := min(int64(4<<20), m.Size-offset)
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err = writer.WriteField("segment_index", strconv.Itoa(index)); err != nil {
			return "", err
		}
		part, err := writer.CreateFormFile("media", "segment")
		if err != nil {
			return "", err
		}
		if _, err = io.CopyN(part, f, count); err != nil {
			return "", fail("permanent", "stored media truncated")
		}
		if err = writer.Close(); err != nil {
			return "", err
		}
		if _, err = exchange(ctx, p.http, http.MethodPost, p.base+root+"/"+key+"/append", p.headers(token), writer.FormDataContentType(), &body, int64(body.Len()), nil, false); err != nil {
			return "", err
		}
		offset += count
	}
	response = xMediaResponse{}
	if _, err = p.json(ctx, http.MethodPost, root+"/"+key+"/finalize", token, nil, &response, false); err != nil {
		return "", err
	}
	for response.Data.Processing != nil {
		status := response.Data.Processing
		switch status.State {
		case "succeeded":
			return key, nil
		case "pending", "in_progress":
		default:
			return "", fail("permanent", "X rejected media during processing")
		}
		if err = pause(ctx, time.Duration(max(1, min(status.Wait, 30)))*time.Second); err != nil {
			return "", err
		}
		response = xMediaResponse{}
		if _, err = p.json(ctx, http.MethodGet, root+"?command=STATUS&media_id="+key, token, nil, &response, false); err != nil {
			return "", err
		}
		if response.Data.Processing == nil {
			return "", unknown(false)
		}
	}
	return key, nil
}

func (p *platform) linkedin(ctx context.Context, t Target, a Account, m Media, token string) (Result, error) {
	payload := fields{
		"author": a.RemoteID, "commentary": bodyText(t), "visibility": "PUBLIC", "lifecycleState": "PUBLISHED",
		"distribution":              fields{"feedDistribution": "MAIN_FEED", "targetEntities": []any{}, "thirdPartyDistributionChannels": []any{}},
		"isReshareDisabledByAuthor": false,
	}
	if m.ID != "" {
		key, err := p.uploadLinkedIn(ctx, a, m, token)
		if err != nil {
			return Result{}, err
		}
		payload["content"] = fields{"media": fields{"id": key}}
	} else if t.LinkTitle != "" {
		payload["commentary"] = t.Text
		payload["content"] = fields{"article": fields{"source": t.URL, "title": t.LinkTitle, "description": t.LinkDescription}}
	}
	h, err := p.json(ctx, http.MethodPost, "/rest/posts", token, payload, nil, true)
	return confirmed(h.Get("X-RestLi-Id"), err)
}

type linkedUpload struct {
	Value struct {
		Image string `json:"image"`
		Video string `json:"video"`
		Token string `json:"uploadToken"`
		URL   string `json:"uploadUrl"`
		Parts []struct {
			URL   string `json:"uploadUrl"`
			First int64  `json:"firstByte"`
			Last  int64  `json:"lastByte"`
		} `json:"uploadInstructions"`
	}
}

func (p *platform) linkedPut(ctx context.Context, address, token string, r io.Reader, size int64) (http.Header, error) {
	u, err := url.Parse(address)
	if err != nil || !validURL(address) {
		return nil, fail("permanent", "provider supplied an invalid upload URL")
	}
	host := strings.ToLower(u.Hostname())
	if host != "www.linkedin.com" && host != "api.linkedin.com" && host != "media.licdn.com" {
		return nil, fail("permanent", "provider upload hostname is unsupported")
	}
	var headers http.Header
	if token != "" {
		headers = bearer(token)
	}
	return exchange(ctx, p.http, http.MethodPut, address, headers, "application/octet-stream", r, size, nil, false)
}
func (p *platform) uploadLinkedIn(ctx context.Context, a Account, m Media, token string) (string, error) {
	f, err := os.Open(m.Path)
	if err != nil {
		return "", fail("permanent", "cannot open stored media")
	}
	defer f.Close()
	endpoint := "/rest/images"
	input := fields{"owner": a.RemoteID}
	if m.MIME == "video/mp4" {
		endpoint = "/rest/videos"
		input["fileSizeBytes"], input["uploadCaptions"], input["uploadThumbnail"] = m.Size, false, false
	}
	var response linkedUpload
	if _, err = p.json(ctx, http.MethodPost, endpoint+"?action=initializeUpload", token, fields{"initializeUploadRequest": input}, &response, false); err != nil {
		return "", err
	}
	info := response.Value
	key := info.Image
	if m.MIME == "video/mp4" {
		key = info.Video
		if !strings.HasPrefix(key, "urn:li:video:") || len(info.Parts) == 0 {
			return "", unknown(false)
		}
		tags := make([]string, 0, len(info.Parts))
		var offset int64
		for _, part := range info.Parts {
			if part.First != offset || part.Last < part.First || part.Last >= m.Size {
				return "", unknown(false)
			}
			count := part.Last - part.First + 1
			h, err := p.linkedPut(ctx, part.URL, "", io.NewSectionReader(f, part.First, count), count)
			if err != nil {
				return "", err
			}
			tag := strings.Trim(h.Get("ETag"), "\"")
			if tag == "" {
				return "", unknown(false)
			}
			tags = append(tags, tag)
			offset += count
		}
		if offset != m.Size {
			return "", unknown(false)
		}
		if _, err = p.json(ctx, http.MethodPost, endpoint+"?action=finalizeUpload", token, fields{"finalizeUploadRequest": fields{"video": key, "uploadToken": info.Token, "uploadedPartIds": tags}}, nil, false); err != nil {
			return "", err
		}
	} else {
		if !strings.HasPrefix(key, "urn:li:image:") {
			return "", unknown(false)
		}
		if _, err = p.linkedPut(ctx, info.URL, token, f, m.Size); err != nil {
			return "", err
		}
		// LinkedIn's write-only member permission cannot read a versioned image.
		if strings.HasPrefix(a.RemoteID, "urn:li:person:") {
			return key, nil
		}
	}
	for {
		var response struct{ Status string }
		path := fmt.Sprintf("%s/%s", endpoint, url.QueryEscape(key))
		if _, err = p.json(ctx, http.MethodGet, path, token, nil, &response, false); err != nil {
			return "", err
		}
		switch response.Status {
		case "AVAILABLE":
			return key, nil
		case "WAITING_UPLOAD", "PROCESSING":
		default:
			return "", fail("permanent", "LinkedIn rejected media during processing")
		}
		if err = pause(ctx, 5*time.Second); err != nil {
			return "", err
		}
	}
}
