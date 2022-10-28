package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/buger/jsonparser"
	"github.com/gorilla/mux"
	"github.com/heroku/docker-registry-client/registry"
	"github.com/novln/docker-parser"
	"io"
	"log"
	"mc-oci-labels/cache"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	dns1123LabelFmt           string = "[a-z0-9]([-a-z0-9]*[a-z0-9])?"
	dns1123SubdomainFmt              = dns1123LabelFmt + "(\\." + dns1123LabelFmt + ")*"
	dNS1123SubdomainMaxLength int    = 253
	labelValueFmt                    = "(" + qualifiedNameFmt + ")?"
	labelValueMaxLength       int    = 63
	qnameCharFmt              string = "[A-Za-z0-9]"
	qnameExtCharFmt           string = "[-A-Za-z0-9_.]"
	qualifiedNameFmt                 = "(" + qnameCharFmt + qnameExtCharFmt + "*)?" + qnameCharFmt
	qualifiedNameMaxLength    int    = 63
)

var (
	dns1123SubdomainRegexp = regexp.MustCompile("^" + dns1123SubdomainFmt + "$")
	labelCache             = cache.New(5*time.Minute, 10*time.Minute)
	labelValueRegexp       = regexp.MustCompile("^" + labelValueFmt + "$")
	qualifiedNameRegexp    = regexp.MustCompile("^" + qualifiedNameFmt + "$")
	registryClients        = map[string]*registry.Registry{}
)

func getImageLabels(podImage string) (map[string]string, error) {
	image, err := dockerparser.Parse(podImage)
	if err != nil {
		return nil, err
	}
	hub, ok := registryClients[image.Registry()]
	if !ok {
		log.Println("no credential for registry", image.Registry(), "skipping resolution")
		return map[string]string{}, nil
	}
	manifest, err := hub.ManifestV2(image.ShortName(), image.Tag())
	if err != nil {
		log.Println(err)
		return map[string]string{}, nil
	}
	reader, err := hub.DownloadBlob(image.ShortName(), manifest.Config.Digest)
	if reader != nil {
		defer reader.Close()
	}
	layer, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	labels, err := getMap(layer, "config", "Labels")
	if err != nil {
		log.Println("no label found for", podImage)
		return map[string]string{}, nil
	}
	return labels, nil
}

func getImageLabelsWithCache(podImage string) (map[string]string, error) {
	if x, found := labelCache.Get(podImage); found {
		return x.(map[string]string), nil
	}
	labelCache.Lock()
	labels, err := getImageLabels(podImage)
	if labels != nil {
		labelCache.SetNoLock(podImage, labels, cache.DefaultExpiration)
	}
	labelCache.Unlock()
	return labels, err
}

func getMap(data []byte, keys ...string) (map[string]string, error) {
	raw, _, _, err := jsonparser.Get(data, keys...)
	if err != nil {
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
	body, err := io.ReadAll(r.Body)
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
	imageLabels, err := getImageLabelsWithCache(podImage)
	if err != nil {
		log.Println(err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Filter labels
	validLabels := make(map[string]string)
	for k, v := range imageLabels {
		if isQualifiedName(k) && isValidLabelValue(v) {
			validLabels[k] = v
		}
	}
	// Return new labels
	response, err := json.Marshal(map[string]map[string]string{
		"labels": validLabels,
	})
	if err != nil {
		log.Println(err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, string(response))
}

func isDNS1123Subdomain(value string) bool {
	if len(value) > dNS1123SubdomainMaxLength {
		return false
	}
	if !dns1123SubdomainRegexp.MatchString(value) {
		return false
	}
	return true
}

func isQualifiedName(value string) bool {
	parts := strings.Split(value, "/")
	var name string
	switch len(parts) {
	case 1:
		name = parts[0]
	case 2:
		var prefix string
		prefix, name = parts[0], parts[1]
		if len(prefix) == 0 {
			return false
		} else {
			return isDNS1123Subdomain(prefix)
		}
	default:
		return false
	}
	if len(name) == 0 {
		return false
	} else if len(name) > qualifiedNameMaxLength {
		return false
	}
	if !qualifiedNameRegexp.MatchString(name) {
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
