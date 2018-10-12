/*
Copyright 2018 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Note: the example only works with the code within the same release/branch.
// This is based on the example from the official k8s golang client repository:
// k8s.io/client-go/examples/create-update-delete-deployment/
package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/open-match/config"
	"github.com/GoogleCloudPlatform/open-match/internal/metrics"
	redisHelpers "github.com/GoogleCloudPlatform/open-match/internal/statestorage/redis"
	"github.com/tidwall/gjson"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"

	"github.com/gomodule/redigo/redis"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	batchv1 "k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	//"k8s.io/kubernetes/pkg/api"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	// Uncomment the following line to load the gcp plugin (only required to authenticate against GKE clusters).
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

var (
	// Logrus structured logging setup
	mmforcLogFields = log.Fields{
		"app":       "openmatch",
		"component": "mmforc",
		"caller":    "mmforc/main.go",
	}
	mmforcLog = log.WithFields(mmforcLogFields)

	// Viper config management setup
	cfg = viper.New()
	err = errors.New("")
)

func init() {
	// Logrus structured logging initialization
	// Add a hook to the logger to auto-count log lines for metrics output thru OpenCensus
	log.SetFormatter(&log.JSONFormatter{})
	log.AddHook(metrics.NewHook(MmforcLogLines, KeySeverity))

	// Viper config management initialization
	cfg, err = config.Read()
	if err != nil {
		mmforcLog.WithFields(log.Fields{
			"error": err.Error(),
		}).Error("Unable to load config file")
	}

	if cfg.GetBool("debug") == true {
		log.SetLevel(log.DebugLevel) // debug only, verbose - turn off in production!
		mmforcLog.Warn("Debug logging configured. Not recommended for production!")
	}

	// Configure OpenCensus exporter to Prometheus
	// metrics.ConfigureOpenCensusPrometheusExporter expects that every OpenCensus view you
	// want to register is in an array, so append any views you want from other
	// packages to a single array here.
	ocMmforcViews := DefaultMmforcViews // mmforc OpenCensus views.
	// Waiting on https://github.com/opencensus-integrations/redigo/pull/1
	// ocMmforcViews = append(ocMmforcViews, redis.ObservabilityMetricViews...) // redis OpenCensus views.
	mmforcLog.WithFields(log.Fields{"viewscount": len(ocMmforcViews)}).Info("Loaded OpenCensus views")
	metrics.ConfigureOpenCensusPrometheusExporter(cfg, ocMmforcViews)

}

func main() {

	// Connect to redis
	// As per https://www.iana.org/assignments/uri-schemes/prov/redis
	// redis://user:secret@localhost:6379/0?foo=bar&qux=baz
	// redis pool docs: https://godoc.org/github.com/gomodule/redigo/redis#Pool
	redisURL := "redis://" + cfg.GetString("redis.hostname") + ":" + cfg.GetString("redis.port")

	mmforcLog.WithFields(log.Fields{"redisURL": redisURL}).Info("Attempting to connect to Redis")
	pool := redis.Pool{
		MaxIdle:     3,
		MaxActive:   0,
		IdleTimeout: 60 * time.Second,
		Dial:        func() (redis.Conn, error) { return redis.DialURL(redisURL) },
	}
	mmforcLog.Info("Connected to Redis")

	redisConn := pool.Get()
	defer redisConn.Close()

	// Get k8s credentials so we can starts k8s Jobs
	mmforcLog.Info("Attempting to acquire k8s credentials")
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}
	mmforcLog.Info("K8s credentials acquired")

	start := time.Now()
	checkProposals := true
	defaultMmfImages := []string{cfg.GetString("defaultImages.mmf.name") + ":" + cfg.GetString("defaultImages.mmf.tag")}
	defaultEvalImage := cfg.GetString("defaultImages.evaluator.name") + ":" + cfg.GetString("defaultImages.evaluator.tag")

	// main loop; kick off matchmaker functions for profiles in the profile
	// queue and an evaluator when proposals are in the proposals queue
	for {
		ctx, cancel := context.WithCancel(context.Background())
		_ = cancel

		// Get profiles and kick off a job for each
		mmforcLog.WithFields(log.Fields{
			"profileQueueName": cfg.GetString("queues.profiles.name"),
			"pullCount":        cfg.GetInt("queues.profiles.pullCount"),
			"query":            "SPOP",
			"component":        "statestorage",
		}).Info("Retreiving match profiles")

		results, err := redis.Strings(redisConn.Do("SPOP",
			cfg.GetString("queues.profiles.name"), cfg.GetInt("queues.profiles.pullCount")))
		if err != nil {
			panic(err)
		}

		if len(results) > 0 {
			mmforcLog.WithFields(log.Fields{
				"numProfiles": len(results),
			}).Info("Starting MMF jobs...")

			for _, profile := range results {
				// Kick off the job asynchrnously
				go mmfunc(ctx, profile, cfg, defaultMmfImages, clientset, &pool)
				// Count the number of jobs running
				redisHelpers.Increment(context.Background(), &pool, "concurrentMMFs")
			}
		} else {
			mmforcLog.WithFields(log.Fields{
				"profileQueueName": cfg.GetString("queues.profiles.name"),
			}).Warn("Unable to retreive match profiles from statestorage - have you entered any?")
		}

		// Check to see if we should run the evaluator.
		// Get number of running MMFs
		r, err := redisHelpers.Retrieve(context.Background(), &pool, "concurrentMMFs")

		if err != nil {
			mmforcLog.Println(err)
			if err.Error() == "redigo: nil returned" {
				// No MMFs have run since we last evaluated; reset timer and loop
				mmforcLog.Debug("Number of concurrentMMFs is nil")
				start = time.Now()
				time.Sleep(1000 * time.Millisecond)
			}
			continue
		}
		numRunning, err := strconv.Atoi(r)
		if err != nil {
			mmforcLog.WithFields(log.Fields{
				"error": err.Error(),
			}).Error("Issue retrieving number of currently running MMFs")
		}

		// We are ready to evaluate either when all MMFs are complete, or the
		// timeout is reached.
		//
		// Tuning how frequently the evaluator runs is a complex topic and
		// probably only of interest to users running large-scale production
		// workloads with many concurrently running matchmaking functions,
		// which have some overlap in the matchmaking player pools. Suffice to
		// say that under load, this switch should almost always trigger the
		// timeout interval code path.  The concurrentMMFs check to see how
		// many are still running is meant as a short-circuit to prevent
		// waiting to run the evaluator when all your MMFs are already
		// finished.
		switch {
		case time.Since(start).Seconds() >= float64(cfg.GetInt("interval.evaluator")):
			mmforcLog.WithFields(log.Fields{
				"interval": cfg.GetInt("interval.evaluator"),
			}).Info("Maximum evaluator interval exceeded")
			checkProposals = true

			// Opencensus tagging
			ctx, _ = tag.New(ctx, tag.Insert(KeyEvalReason, "interval_exceeded"))
		case numRunning <= 0:
			mmforcLog.Info("All MMFs complete")
			checkProposals = true
			numRunning = 0
			ctx, _ = tag.New(ctx, tag.Insert(KeyEvalReason, "mmfs_completed"))
		}

		if checkProposals {
			// Make sure there are proposals in the queue.
			checkProposals = false
			mmforcLog.Info("Checking statestorage for match object proposals")
			results, err := redisHelpers.Count(context.Background(), &pool, cfg.GetString("queues.proposals.name"))
			switch {
			case err != nil:
				mmforcLog.WithFields(log.Fields{
					"error": err.Error(),
				}).Error("Couldn't retrieve the length of the proposal queue from statestorage!")
			case results == 0:
				mmforcLog.WithFields(log.Fields{}).Warn("No proposals in the queue!")
			default:
				mmforcLog.WithFields(log.Fields{
					"numProposals": results,
				}).Info("Proposals available, evaluating!")
				go evaluator(ctx, cfg, defaultEvalImage, clientset)
			}
			_, err = redisHelpers.Delete(context.Background(), &pool, "concurrentMMFs")
			if err != nil {
				mmforcLog.WithFields(log.Fields{
					"error": err.Error(),
				}).Error("Error deleting concurrent MMF counter!")
			}
			start = time.Now()
		}

		// TODO: Make this tunable via config.
		// A sleep here is not critical but just a useful safety valve in case
		// things are broken, to keep the main loop from going all-out and spamming the log.
		mainSleep := 1000
		mmforcLog.WithFields(log.Fields{
			"ms": mainSleep,
		}).Info("Sleeping...")
		time.Sleep(time.Duration(mainSleep) * time.Millisecond)
	} // End main for loop
}

// mmfunc generates a k8s job that runs the specified mmf container image.
func mmfunc(ctx context.Context, profile string, cfg *viper.Viper, imageNames []string, clientset *kubernetes.Clientset, pool *redis.Pool) {
	// Generate the various keys/names, some of which must be populated to the k8s job.
	ids := strings.Split(profile, ".")
	moID := ids[0]
	proID := ids[1]
	timestamp := strconv.Itoa(int(time.Now().Unix()))
	jobName := timestamp + "." + moID + "." + proID

	// Read the full profile from redis and access any keys that are important to deciding how MMFs are run.
	profile, err := redisHelpers.Retrieve(ctx, pool, proID)
	if err != nil {
		// Note that we couldn't read the profile, and try to run the mmf with default settings.
		mmforcLog.WithFields(log.Fields{
			"error":           err.Error(),
			"jobName":         moID,
			"profile":         proID,
			"containerImages": imageNames,
		}).Warn("Failure retreiving full profile from statestorage - attempting to run default mmf container")
	} else {
		profileImageNames := gjson.Get(profile, cfg.GetString("jsonkeys.mmfImages"))

		// Got profile from state storage, make sure it is valid
		if gjson.Valid(profile) && profileImageNames.Exists() {
			switch profileImageNames.Type.String() {
			case "String":
				// case: only one image name at this key.
				imageNames = []string{profileImageNames.String()}
			case "JSON":
				// case: Array of image names at this key.
				// TODO: support multiple MMFs per profile.  Doing this will require that
				// we generate an proposal ID and populate it to the env vars for each
				// mmf, so they can each write a proposal for the same profile
				// without stomping each other. (The evaluator would then be
				// responsible for selecting the proposal to send to the backendapi)
				imageNames = []string{}

				// Pattern for iterating through a gjson.Result
				// https://github.com/tidwall/gjson#iterate-through-an-object-or-array
				profileImageNames.ForEach(func(_, name gjson.Result) bool {
					// TODO: Swap these two lines when multiple image support is ready
					// imageNames = append(imageNames, name.String())
					imageNames = []string{name.String()}
					return true
				})
				mmforcLog.WithFields(log.Fields{
					"jobName":         moID,
					"profile":         proID,
					"containerImages": imageNames,
				}).Warn("Profile specifies multiple MMF container images (NYI), running only the last image provided")
			}
		} else {
			mmforcLog.WithFields(log.Fields{
				"jobName":         moID,
				"profile":         proID,
				"containerImages": imageNames,
			}).Warn("Profile JSON was invalid or did not contain a MMF container image name - attempting to run default mmf container")
		}
	}
	mmforcLog.WithFields(log.Fields{
		"jobName":        moID,
		"profile":        proID,
		"containerImage": imageNames,
	}).Info("Attempting to create mmf k8s job")

	// Create Jobs
	// TODO: Handle returned errors
	// TODO: Support multiple MMFs per profile.
	// NOTE: For now, always send this an array of length 1 specifying the
	// single MMF container image name you want to run, until multi-mmf
	// profiles are supported. If you send it more than one, you will get
	// undefined (but definitely degenerate) behavior!
	for _, imageName := range imageNames {
		// Kick off Job with this image name
		_ = submitJob(imageName, jobName, clientset)
		if err != nil {
			// Record failure & log
			stats.Record(ctx, mmforcMmfFailures.M(1))
			mmforcLog.WithFields(log.Fields{
				"error":          err.Error(),
				"jobName":        moID,
				"profile":        proID,
				"containerImage": imageName,
			}).Error("MMF job submission failure!")
		} else {
			// Record Success
			stats.Record(ctx, mmforcMmfs.M(1))
		}
	}
}

// evaluator generates a k8s job that runs the specified evaluator container image.
func evaluator(ctx context.Context, cfg *viper.Viper, imageName string, clientset *kubernetes.Clientset) {
	// Generate the job name
	timestamp := strconv.Itoa(int(time.Now().Unix()))
	jobName := timestamp + ".evaluator"

	mmforcLog.WithFields(log.Fields{
		"jobName":        jobName,
		"containerImage": imageName,
	}).Info("Attempting to create evaluator k8s job")

	// Create Job
	// TODO: Handle returned errors
	_ = submitJob(imageName, jobName, clientset)
	if err != nil {
		// Record failure & log
		stats.Record(ctx, mmforcEvalFailures.M(1))
		mmforcLog.WithFields(log.Fields{
			"error":          err.Error(),
			"jobName":        jobName,
			"containerImage": imageName,
		}).Error("Evaluator job submission failure!")
	} else {
		// Record success
		stats.Record(ctx, mmforcEvals.M(1))
	}
}

// submitJob submits a job to kubernetes
func submitJob(imageName string, jobName string, clientset *kubernetes.Clientset) error {
	job := generateJobSpec(jobName, imageName)

	// Get the namespace for the job from the current namespace, otherwise, use default
	namespace := os.Getenv("METADATA_NAMESPACE")
	if len(namespace) == 0 {
		namespace = apiv1.NamespaceDefault
	}

	// Submit kubernetes job
	jobsClient := clientset.BatchV1().Jobs(namespace)
	result, err := jobsClient.Create(job)
	if err != nil {
		// TODO: replace queued profiles if things go south
		mmforcLog.WithFields(log.Fields{
			"error": err.Error(),
		}).Error("Couldn't create k8s job!")
	}

	mmforcLog.WithFields(log.Fields{
		"jobName": result.GetObjectMeta().GetName(),
	}).Info("Created job.")

	return err
}

// generateJobSpec is a PoC to test that all the k8s job generation code works.
// In the future we should be decoding into the client object using one of the
// codecs on an input JSON, or piggyback on job templates.
// https://github.com/kubernetes/client-go/issues/193
// TODO: many fields in this job spec assume the container image is an mmf, but
// we use this to kick the evaluator containers too, should be updated to
// reflect that
func generateJobSpec(jobName string, imageName string) *batchv1.Job {

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: jobName,
		},
		Spec: batchv1.JobSpec{
			Completions: int32Ptr(1),
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "mmf", // TODO: have this reflect mmf vs evaluator
					},
					Annotations: map[string]string{
						// Unused; here as an example.
						// Later we can put params here and read them using the
						// k8s downward API volumes
						"profile": "exampleprofile",
					},
				},
				Spec: apiv1.PodSpec{
					RestartPolicy: "Never",
					ImagePullSecrets: []apiv1.LocalObjectReference{
						{
							Name: "aws-creds",
						},
					},
					Containers: []apiv1.Container{
						{
							// TODO: have these reflect mmf vs evaluator
							Name:            "mmf",
							Image:           imageName,
							ImagePullPolicy: "Always",
							Env: []apiv1.EnvVar{
								{
									Name:  "PROFILE",
									Value: jobName,
								},
							},
						},
					},
				},
			},
		},
	}
	return job
}

// readability functions used by generateJobSpec
func int32Ptr(i int32) *int32 { return &i }
func strPtr(i string) *string { return &i }
