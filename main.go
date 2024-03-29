package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os/exec"
	"strings"
	"time"

	"net/http"

	"github.com/Workiva/go-datastructures/queue"
)

type empty struct{}
type semaphore chan empty

func (s semaphore) enter(n int) {
	e := empty{}
	for i := 0; i < n; i++ {
		s <- e
	}
}

func (s semaphore) leave(n int) {
	for i := 0; i < n; i++ {
		<-s
	}
}

var repoCommands = make(map[string][]string)
var dispatcherQueues = make(map[string]*queue.Queue)

// DockerHubPayload is (partially) the webhook produced when a build completes on hub.docker.com
type DockerHubPayload struct {
	CallbackURL string              `json:"callback_url,omitempty"`
	Repository  DockerHubRepository `json:"repository,omitempty"`
	PushData    DockerPushData      `json:"push_data,omitempty"`
}

// DockerPushData is push info
type DockerPushData struct {
	Pusher string `json:"pusher,omitempty"`
	Tag    string `json:"tag,omitempty"`
}

// DockerHubRepository Some of the pertinent bits that we'll use
type DockerHubRepository struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Owner     string `json:"owner,omitempty"`
	RepoName  string `json:"repo_name,omitempty"`
	RepoUrl   string `json:"repo_url,omitempty"`
}

type JobContext struct {
	Payload DockerHubPayload
	Command string
	Args    []string
}

// DockerHubHandler accepts the webhook payload as produced when a build completes on hub.docker.com
func DockerHubHandler(res http.ResponseWriter, req *http.Request) {
	notAuthorized := true

	if username, password, ok := req.BasicAuth(); ok && username == basicAuthUsername && password == basicAuthPassword {
		notAuthorized = false
	}

	if notAuthorized {
		res.Header().Set("WWW-Authenticate", `Basic realm="PRIVATE AREA"`)
		http.Error(res, "401 Unauthorized.", http.StatusUnauthorized)
		return
	}

	if req.Method != "POST" {
		http.Error(res, "Not supported method.", http.StatusBadRequest)
		return
	}

	decoder := json.NewDecoder(req.Body)
	var dockerHubPayload DockerHubPayload
	err := decoder.Decode(&dockerHubPayload)

	if err != nil {
		log.Printf("Unable to read payload as JSON, %s\n", err)
		return
	}

	repo := dockerHubPayload.Repository.RepoName
	command, ok := repoCommands[repo]

	if !ok { // || len(command) == 0 {
		log.Printf("Repository \"%s\" not enabled\n", repo)
		return
	}

	DispatchJob(JobContext{
		Payload: dockerHubPayload,
		Command: command[0],
		Args:    command[1:],
	})
}

func DispatchJob(job JobContext) {
	repo := job.Payload.Repository.RepoName
	if queue, ok := dispatcherQueues[repo]; !ok {
		log.Printf("Repository \"%s\" not enabled\n", repo)
	} else {
		queue.Put(job)
	}
}

var (
	workers                              uint
	listenPort                           uint
	listenIp, configFile                 string
	basicAuthUsername, basicAuthPassword string
	basicAuthFile                        string
)

func main() {


	flag.UintVar(&workers, "workers", 2, "Workers count")
	flag.UintVar(&listenPort, "listen-port", 8080, "Listen Port")
	flag.StringVar(&listenIp, "listen-ip", "0.0.0.0", "Listen IP")
	flag.StringVar(&configFile, "config", "config.json", "Repository")
	flag.StringVar(&basicAuthUsername, "http-auth-user", "user", "Http Basic Auth username")
	flag.StringVar(&basicAuthPassword, "http-auth-pass", "pass", "Http Basic Auth password")
	flag.StringVar(&basicAuthFile, "http-auth-file", "", "Http Basic Auth credentials file (like 'username:password' no LR, CR, etc)")

	flag.Parse()

	if len(basicAuthFile) > 0 {
		if credFileBody, err := ioutil.ReadFile(basicAuthFile); err == nil {
			splittedItems := strings.Split(string(credFileBody), ":")
			if len(splittedItems) != 2 {
				log.Fatalf("Invalid credential file format")
			}
			basicAuthUsername, basicAuthPassword = splittedItems[0], splittedItems[1]
		} else {
			log.Fatalf("Can't read credential file %s", err)
		}
	}

	if configFileBody, err := ioutil.ReadFile(configFile); err == nil {
		err = json.Unmarshal(configFileBody, &repoCommands)
		if err != nil {
			log.Fatalf("Can't parse file %s", err)
		}
	} else {
		log.Fatalf("Can't read file %s", err)
	}

	if len(repoCommands) == 0 {
		log.Fatalf("No repositories configured")
	}

	var workerLimiter = make(semaphore, workers)

	for repName := range repoCommands {
		dispatcherQueues[repName] = queue.New(1)

		go func(q *queue.Queue) {
			for {

				jobs, err := q.Get(1)
				workerLimiter.enter(1)

				job := jobs[0].(JobContext)

				repName := job.Payload.Repository.RepoName
				log.Printf("Processing: %s %s", repName, job.Command)

				args := make([]string, 0)
				for _, arg := range job.Args {
					replacedArg := arg

					replacedArg = strings.Replace(replacedArg, "${repository.repo_url}", job.Payload.Repository.RepoUrl, -1)
					replacedArg = strings.Replace(replacedArg, "${repository.repo_name}", job.Payload.Repository.RepoName, -1)
					replacedArg = strings.Replace(replacedArg, "${repository.name}", repName, -1)
					replacedArg = strings.Replace(replacedArg, "${repository.namespace}", job.Payload.Repository.Namespace, -1)
					replacedArg = strings.Replace(replacedArg, "${repository.owner}", job.Payload.Repository.Owner, -1)
					replacedArg = strings.Replace(replacedArg, "${push_data.tag}", job.Payload.PushData.Tag, -1)
					replacedArg = strings.Replace(replacedArg, "${push_data.pusher}", job.Payload.PushData.Pusher, -1)

					args = append(args, replacedArg)
				}

				repoCmd := exec.Command(job.Command, args...)

				output, err := repoCmd.Output()
				if err != nil {
					log.Printf("ERROR: %s %s %s %s\n", repName, job.Command, args, err)
				}
				log.Printf("Output: %s %s %s", repName, job.Command, output)

				time.Sleep(time.Second)

				workerLimiter.leave(1)
			}
		}(dispatcherQueues[repName])
	}

	log.Println("Docker repository listeners:")
	for repo, _ := range repoCommands {
		log.Printf("\t%s\n", repo)
	}

	log.Println("Point your Hook config at: http://{IP+Port}/autodock/v1/")

	http.HandleFunc("/autodock/v1", DockerHubHandler)
	http.HandleFunc("/autodock/v1/", DockerHubHandler)
	log.Fatal(http.ListenAndServe(fmt.Sprintf("%s:%d", listenIp, listenPort), nil))
}
