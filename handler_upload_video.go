package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	uploadLimit := http.MaxBytesReader(w, r.Body, 1<<30)
	r.Body = uploadLimit
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to get video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unable to get video", nil)
		return
	}
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()
	mediaType := header.Header.Get("Content-Type")

	mimeType, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse media type", err)
		return
	}
	if mimeType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Incorrect media type", nil)
		return
	}
	tmpFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to create temp file", err)
		return
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()
	io.Copy(tmpFile, file)
	tmpFile.Seek(0, io.SeekStart)
	processed, err := processVideoForFastStart(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video", err)
		return
	}
	processedFile, err := os.Open(processed)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video", err)
		return
	}
	defer os.Remove(processedFile.Name())
	defer processedFile.Close()

	mimeTypeSplit := strings.Split(mimeType, "/")
	extension := mimeTypeSplit[1]
	randomBytes := make([]byte, 32)
	_, err = rand.Read(randomBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating url", err)
		return
	}
	aspectRatio, err := getVideoAspectRatio(processedFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting aspect ratio", err)
		return
	}
	randomString := base64.RawURLEncoding.EncodeToString(randomBytes)
	key := fmt.Sprintf("%v/%v.%v", aspectRatio, randomString, extension)

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{Bucket: &cfg.s3Bucket, Key: &key, Body: processedFile, ContentType: &mimeType})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error adding object to bucket", err)
		return
	}

	url := fmt.Sprintf("https://%v/%v", cfg.s3CfDistribution, key)

	video.VideoURL = &url
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	buffer := &bytes.Buffer{}
	cmd.Stdout = buffer
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	type parameters struct {
		Streams []struct {
			Height int `json:"height"`
			Width  int `json:"width"`
		} `json:"streams"`
	}
	var params parameters
	err = json.Unmarshal(buffer.Bytes(), &params)
	if err != nil {
		return "", err
	}
	height := params.Streams[0].Height
	width := params.Streams[0].Width
	ratio := float64(width) / float64(height)
	if math.Abs(ratio-16.0/9.0) < 0.1 {
		return "landscape", nil
	} else if math.Abs(ratio-9.0/16.0) < 0.1 {
		return "portrait", nil
	} else {
		return "other", nil
	}
}

func processVideoForFastStart(filePath string) (string, error) {
	newPath := filePath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", newPath)
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return newPath, nil
}
