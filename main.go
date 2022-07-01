package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/buger/jsonparser"
	"github.com/gorilla/mux"
	"github.com/heroku/docker-registry-client/registry"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	dns1123LabelFmt       string = "[a-z0-9]([-a-z0-9]*[a-z0-9])?"
	dNS1123LabelMaxLength int    = 63
	labelValueFmt                = "(" + qualifiedNameFmt + ")?"
	labelValueMaxLength   int    = 63
	qnameCharFmt          string = "[A-Za-z0-9]"
	qnameExtCharFmt       string = "[-A-Za-z0-9_.]"
	qualifiedNameFmt             = "(" + qnameCharFmt + qnameExtCharFmt + "*)?" + qnameCharFmt
)

var (
	dns1123LabelRegexp = regexp.MustCompile("^" + dns1123LabelFmt + "$")
	imageUrl           = regexp.MustCompile("([^/]+)/([^:]+):(\\S+)")
	labelValueRegexp   = regexp.MustCompile("^" + labelValueFmt + "$")
	registryClients    = map[string]*registry.Registry{}
)

func getImageLabels(podImage string) (map[string]string, error) {
	podImageMatch := imageUrl.FindStringSubmatch(podImage)
	if podImageMatch == nil {
		return nil, fmt.Errorf("cannot parse image name %v", podImage)
	}
	registryHost := podImageMatch[1]
	repository := podImageMatch[2]
	reference := podImageMatch[3]
	hub, ok := registryClients[registryHost]
	if !ok {
		return map[string]string{}, nil
	}
	manifest, err := hub.ManifestV2(repository, reference)
	if err != nil {
		return nil, err
	}
	reader, err := hub.DownloadBlob(repository, manifest.Config.Digest)
	if reader != nil {
		defer reader.Close()
	}
	image, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	return getMap(image, "config", "Labels")
}

func getMap(data []byte, keys ...string) (map[string]string, error) {
	raw, _, _, err := jsonparser.Get(data, keys...)
	if err != nil {
		log.Println(string(data))
		return nil, fmt.Errorf("error getting %v %v", keys, err)
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
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Println(err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	podImage, err := jsonparser.GetString(body, "object", "spec", "containers", "[0]", "image")
	if err != nil {
		log.Println("error getting spec/containers[0]/image", err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Get labels
	imageLabels, err := getImageLabels(podImage)
	if err != nil {
		log.Println(err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Filter labels
	for k, v := range imageLabels {
		if !isValidLabelKey(k) || !isValidLabelValue(v) {
			delete(imageLabels, k)
		}
	}
	// Return new labels
	response, err := json.Marshal(map[string]map[string]string{
		"labels": imageLabels,
	})
	if err != nil {
		log.Println(err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, string(response))
}

func isValidLabelKey(value string) bool {
	if len(value) > dNS1123LabelMaxLength {
		return false
	}
	if !dns1123LabelRegexp.MatchString(value) {
		return false
	}
	return true
}

func isValidLabelValue(value string) bool {
	if len(value) > labelValueMaxLength {
		return false
	}
	if !labelValueRegexp.MatchString(value) {
		return false
	}
	return true
}

func parseDockerCredentials() error {
	dockerconfig, err := os.ReadFile(".dockerconfigjson")
	if err != nil {
		return err
	}
	return jsonparser.ObjectEach(dockerconfig, parseDockerCredential, "auths")
}

func parseDockerCredential(key []byte, value []byte, _ jsonparser.ValueType, _ int) error {
	reg := string(key)
	log.Println("logging into registry", reg)
	raw, err := jsonparser.GetString(value, "auth")
	if err != nil {
		return err
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return err
	}
	s := strings.Split(string(decoded), ":")
	if len(s) != 2 {
		return fmt.Errorf("invalid credential format, should be username:password base64 encoded")
	}
	hub, err := registry.New("https://"+reg, s[0], s[1])
	if err != nil {
		log.Println("error creating registry client", err)
		log.Println("will skip label queries for", reg)
	}
	registryClients[reg] = hub
	return nil
}

func ping(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "OK")
}

func main() {
	err := parseDockerCredentials()
	if err != nil {
		log.Fatal(err.Error())
	}
	r := mux.NewRouter()
	r.HandleFunc("/", handler).Methods("POST")
	r.HandleFunc("/ping", ping)
	addr := ":8000"
	srv := &http.Server{
		Handler: r,
		Addr:    addr,
		// Good practice: enforce timeouts for servers you create!
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}
	log.Println("listening on", addr)
	log.Fatal(srv.ListenAndServe())
}
