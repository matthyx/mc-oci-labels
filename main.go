package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/buger/jsonparser"
	"github.com/gorilla/mux"
	"github.com/heroku/docker-registry-client/registry"
	"golang.org/x/exp/maps"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

var (
	imageUrl        = regexp.MustCompile("([^/]??)/?([^:]+):(\\w+)")
	registryClients = map[string]*registry.Registry{}
)

func dockerCredentials(server string) (string, string, error) {
	dockerconfig, err := os.ReadFile(".dockerconfigjson")
	if err != nil {
		return "", "", err
	}
	raw, err := jsonparser.GetString(dockerconfig, "auths", server, "auth")
	if raw == "" {
		log.Println("Using empty credentials for", server)
		return "", "", nil
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", "", err
	}
	s := strings.Split(string(decoded), ":")
	if len(s) != 2 {
		return "", "", fmt.Errorf("invalid credential format, should be username:password base64 encoded")
	}
	return s[0], s[1], nil
}

func getImageLabels(podImage string) (map[string]string, error) {
	podImageMatch := imageUrl.FindStringSubmatch(podImage)
	if podImageMatch == nil {
		return nil, fmt.Errorf("invalid image format %v", podImage)
	}
	registryHost := podImageMatch[1]
	if registryHost == "" {
		registryHost = "registry-1.docker.io"
	}
	repository := podImageMatch[2]
	reference := podImageMatch[3]
	url := "https://" + registryHost
	hub, ok := registryClients[registryHost]
	if !ok {
		log.Println("Creating registry client for", registryHost)
		username, password, err := dockerCredentials(registryHost)
		if err != nil {
			return nil, err
		}
		hub, err = registry.New(url, username, password)
		if err != nil {
			return nil, err
		}
		registryClients[registryHost] = hub
	}
	manifest, err := hub.ManifestV2(repository, reference)
	if err != nil {
		return nil, err
	}
	reader, err := hub.DownloadBlob(repository, manifest.Config.Digest)
	if reader != nil {
		defer reader.Close()
	}
	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(reader)
	if err != nil {
		return nil, err
	}
	image := buf.Bytes()
	return getMap(image, "config", "Labels")
}

func getMap(data []byte, keys ...string) (map[string]string, error) {
	raw, _, _, err := jsonparser.Get(data, keys...)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	err = json.Unmarshal(raw, &m)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func handler(w http.ResponseWriter, r *http.Request) {
	// Read pod
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(r.Body)
	if err != nil {
		log.Println(err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pod := buf.Bytes()
	podImage, err := jsonparser.GetString(pod, "spec", "containers", "[0]", "image")
	if err != nil {
		log.Println(err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	imageLabels, err := getImageLabels(podImage)
	if err != nil {
		log.Println(err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	podLabels, err := getMap(pod, "metadata", "labels")
	if err != nil {
		log.Println(err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Merge labels
	maps.Copy(podLabels, imageLabels)
	// Save back to pod
	podLabelsRaw, err := json.Marshal(podLabels)
	if err != nil {
		log.Println(err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pod, err = jsonparser.Set(pod, podLabelsRaw, "metadata", "labels")
	if err != nil {
		log.Println(err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Return pod
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, string(pod))
}

func ping(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "OK")
}

func main() {
	r := mux.NewRouter()
	r.HandleFunc("/", handler).Methods("POST")
	r.HandleFunc("/ping", ping)
	addr := "127.0.0.1:8000"
	srv := &http.Server{
		Handler: r,
		Addr:    addr,
		// Good practice: enforce timeouts for servers you create!
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}
	log.Println("Listening on", addr)
	log.Fatal(srv.ListenAndServe())
}
