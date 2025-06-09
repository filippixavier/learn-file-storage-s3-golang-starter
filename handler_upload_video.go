package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func getVideoAspectRatio(filepath string) (string, error) {
	command := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filepath)
	var buffer bytes.Buffer
	var meta VideoMeta
	command.Stdout = &buffer
	err := command.Run()

	if err != nil {
		return "", err
	}

	err = json.Unmarshal(buffer.Bytes(), &meta)

	if err != nil {
		return "", err
	}

	for _, streamInfo := range meta.Streams {
		if streamInfo.CodecType != "video" {
			continue
		}

		if streamInfo.DisplayAspectRatio == "16:9" || streamInfo.DisplayAspectRatio == "9:16" {
			return streamInfo.DisplayAspectRatio, nil
		}
	}

	return "other", nil
}

func processVideoForFastStart(filepath string) (string, error) {
	output := filepath + ".processing"
	command := exec.Command("ffmpeg", "-i", filepath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", output)

	err := command.Run()

	if err != nil {
		return "", err
	}

	fileInfo, err := os.Stat(output)
	if err != nil {
		return "", fmt.Errorf("could not stat processed file: %v", err)
	}
	if fileInfo.Size() == 0 {
		return "", fmt.Errorf("processed file is empty")
	}

	return output, nil
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
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
		respondWithError(w, http.StatusBadRequest, "No video corresponding to videoID", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "User is not the owner of the video", err)
		return
	}

	fmt.Println("uploading video", videoID, "by user", userID)

	uploadLimit := 1 << 30

	r.ParseMultipartForm(int64(uploadLimit))

	uploadedVideo, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}

	defer uploadedVideo.Close()
	defer r.MultipartForm.RemoveAll()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))

	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}

	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", nil)
		return
	}

	tmpFile, err := os.CreateTemp("", "tubely-upload.mp4")

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error when creating temp file", err)
		return
	}

	_, err = io.Copy(tmpFile, uploadedVideo)

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error when writing temp video file", err)
		return
	}

	tmpFile.Seek(0, io.SeekStart)

	ratio, err := getVideoAspectRatio(tmpFile.Name())

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error when fetching video ratio", err)
		return
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	if ratio == "16:9" {
		ratio = "landscape"
	} else if ratio == "9:16" {
		ratio = "portrait"
	}

	processed, err := processVideoForFastStart(tmpFile.Name())

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error when converting video for streaming", err)
		return
	}
	defer os.Remove(processed)

	processedFile, err := os.Open(processed)

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error when converting video for streaming", err)
		return
	}

	defer processedFile.Close()

	key := fmt.Sprintf("%v/%v", ratio, getAssetPath(mediaType))

	_, err = cfg.s3Client.PutObject(context.Background(),
		&s3.PutObjectInput{
			Bucket:      &cfg.s3Bucket,
			Key:         &key,
			Body:        processedFile,
			ContentType: &mediaType,
		})

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error when sending file to s3", err)
		return
	}

	videoURL := fmt.Sprintf("https://%v/%v", cfg.s3CfDistribution, key)

	video.VideoURL = &videoURL

	err = cfg.db.UpdateVideo(video)

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error when updating video", err)
		return
	}

	respondWithJSON(w, 200, video)
}
