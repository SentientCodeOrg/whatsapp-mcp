package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"time"

	"go.mau.fi/whatsmeow"
)

// groupPhotoSide is the size WhatsApp's own apps upload a group photo at.
const groupPhotoSide = 640

// squareJPEG center-crops an image to a square, scales it down to at most
// side×side by averaging the pixels that fall in each output pixel, and
// encodes it as JPEG: the only format WhatsApp takes for a group photo.
func squareJPEG(src image.Image, side int) ([]byte, error) {
	b := src.Bounds()
	n := min(b.Dx(), b.Dy())
	if n == 0 {
		return nil, fmt.Errorf("the image is empty")
	}
	x0, y0 := b.Min.X+(b.Dx()-n)/2, b.Min.Y+(b.Dy()-n)/2
	d := min(n, side)
	sum := make([][4]uint64, d*d) // r, g, b, count
	for y := 0; y < n; y++ {
		row := y * d / n * d
		for x := 0; x < n; x++ {
			r, g, bl, _ := src.At(x0+x, y0+y).RGBA()
			s := &sum[row+x*d/n]
			s[0] += uint64(r)
			s[1] += uint64(g)
			s[2] += uint64(bl)
			s[3]++
		}
	}
	out := image.NewRGBA(image.Rect(0, 0, d, d))
	for i, s := range sum {
		out.Pix[i*4] = uint8(s[0] / s[3] >> 8)
		out.Pix[i*4+1] = uint8(s[1] / s[3] >> 8)
		out.Pix[i*4+2] = uint8(s[2] / s[3] >> 8)
		out.Pix[i*4+3] = 0xff
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// registerGroupPhotoEndpoint adds POST /api/group/photo {group, image_path}
// or {group, remove: true}. The image must sit in an allowed media root, as
// for /api/send.
func registerGroupPhotoEndpoint(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, client *whatsmeow.Client, allowedMediaRoots []string) {
	mux.HandleFunc("/api/group/photo", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Group     string `json:"group"`
			ImagePath string `json:"image_path"`
			Remove    bool   `json:"remove"`
		}
		fail := jsonFail(w)
		if !decodePost(w, r, &req) {
			return
		}
		group, err := parseGroupJID(req.Group)
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		if req.Remove == (req.ImagePath != "") {
			fail(http.StatusBadRequest, "give either image_path or remove: true")
			return
		}
		var photo []byte // nil removes the photo
		if !req.Remove {
			path, err := validateMediaPath(req.ImagePath, allowedMediaRoots)
			if err != nil {
				fail(http.StatusBadRequest, err.Error())
				return
			}
			f, err := os.Open(path)
			if err != nil {
				fail(http.StatusBadRequest, err.Error())
				return
			}
			img, _, err := image.Decode(f)
			f.Close()
			if err != nil {
				fail(http.StatusBadRequest, "the image must be a JPEG, PNG or GIF")
				return
			}
			if photo, err = squareJPEG(img, groupPhotoSide); err != nil {
				fail(http.StatusBadRequest, err.Error())
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		id, err := client.SetGroupPhoto(ctx, group, photo)
		if err != nil {
			fail(http.StatusBadGateway, fmt.Sprintf("changing the photo failed: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "picture_id": id})
	}))
}
