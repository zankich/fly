package integration_test

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
	"github.com/onsi/gomega/ghttp"
	"github.com/vito/go-sse/sse"

	"github.com/concourse/atc"
	"github.com/concourse/atc/event"
	"github.com/concourse/fly/rc"
)

var _ = Describe("Fly CLI", func() {
	var tmpdir string
	var buildDir string
	var taskConfigPath string

	var atcServer *ghttp.Server
	var streaming chan struct{}
	var events chan atc.Event
	var uploadingBits <-chan struct{}

	var expectedPlan atc.Plan

	BeforeEach(func() {
		var err error
		tmpdir, err = ioutil.TempDir("", "fly-build-dir")
		Expect(err).NotTo(HaveOccurred())

		buildDir = filepath.Join(tmpdir, "fixture")

		err = os.Mkdir(buildDir, 0755)
		Expect(err).NotTo(HaveOccurred())

		taskConfigPath = filepath.Join(buildDir, "task.yml")

		err = ioutil.WriteFile(
			taskConfigPath,
			[]byte(`---
platform: some-platform

image: ubuntu

inputs:
- name: fixture

params:
  FOO: bar
  BAZ: buzz
  X: 1

run:
  path: find
  args: [.]
`),
			0644,
		)
		Expect(err).NotTo(HaveOccurred())

		atcServer = ghttp.NewServer()

		streaming = make(chan struct{})
		events = make(chan atc.Event)

		expectedPlan = atc.Plan{
			OnSuccess: &atc.OnSuccessPlan{
				Step: atc.Plan{
					Aggregate: &atc.AggregatePlan{
						atc.Plan{
							Location: &atc.Location{
								ParallelGroup: 1,
								ParentID:      0,
								ID:            2,
							},
							Get: &atc.GetPlan{
								Name: filepath.Base(buildDir),
								Type: "archive",
								Source: atc.Source{
									"uri": atcServer.URL() + "/api/v1/pipes/some-pipe-id",
								},
							},
						},
					},
				},
				Next: atc.Plan{
					Location: &atc.Location{
						ParallelGroup: 0,
						ParentID:      0,
						ID:            3,
					},
					Task: &atc.TaskPlan{
						Name: "one-off",
						Config: &atc.TaskConfig{
							Platform: "some-platform",
							Image:    "ubuntu",
							Inputs: []atc.TaskInputConfig{
								{Name: "fixture"},
							},
							Params: map[string]string{
								"FOO": "bar",
								"BAZ": "buzz",
								"X":   "1",
							},
							Run: atc.TaskRunConfig{
								Path: "find",
								Args: []string{"."},
							},
						},
					},
				},
			},
		}
	})

	AfterEach(func() {
		os.RemoveAll(tmpdir)
	})

	JustBeforeEach(func() {
		uploading := make(chan struct{})
		uploadingBits = uploading

		atcServer.RouteToHandler("POST", "/api/v1/pipes",
			ghttp.CombineHandlers(
				ghttp.VerifyRequest("POST", "/api/v1/pipes"),
				ghttp.RespondWithJSONEncoded(http.StatusCreated, atc.Pipe{
					ID: "some-pipe-id",
				}),
			),
		)
		atcServer.RouteToHandler("POST", "/api/v1/builds",
			ghttp.CombineHandlers(
				ghttp.VerifyRequest("POST", "/api/v1/builds"),
				ghttp.VerifyJSONRepresenting(expectedPlan),
				func(w http.ResponseWriter, r *http.Request) {
					http.SetCookie(w, &http.Cookie{
						Name:    "Some-Cookie",
						Value:   "some-cookie-data",
						Path:    "/",
						Expires: time.Now().Add(1 * time.Minute),
					})
				},
				ghttp.RespondWith(201, `{"id":128}`),
			),
		)
		atcServer.RouteToHandler("GET", "/api/v1/builds/128/events",
			ghttp.CombineHandlers(
				ghttp.VerifyRequest("GET", "/api/v1/builds/128/events"),
				func(w http.ResponseWriter, r *http.Request) {
					flusher := w.(http.Flusher)

					w.Header().Add("Content-Type", "text/event-stream; charset=utf-8")
					w.Header().Add("Cache-Control", "no-cache, no-store, must-revalidate")
					w.Header().Add("Connection", "keep-alive")

					w.WriteHeader(http.StatusOK)

					flusher.Flush()

					close(streaming)

					id := 0

					for e := range events {
						payload, err := json.Marshal(event.Message{Event: e})
						Expect(err).NotTo(HaveOccurred())

						event := sse.Event{
							ID:   fmt.Sprintf("%d", id),
							Name: "event",
							Data: payload,
						}

						err = event.Write(w)
						Expect(err).NotTo(HaveOccurred())

						flusher.Flush()

						id++
					}

					err := sse.Event{
						Name: "end",
					}.Write(w)
					Expect(err).NotTo(HaveOccurred())
				},
			),
		)
		atcServer.RouteToHandler("PUT", "/api/v1/pipes/some-pipe-id",
			ghttp.CombineHandlers(
				ghttp.VerifyRequest("PUT", "/api/v1/pipes/some-pipe-id"),
				func(w http.ResponseWriter, req *http.Request) {
					close(uploading)

					gr, err := gzip.NewReader(req.Body)
					Expect(err).NotTo(HaveOccurred())

					tr := tar.NewReader(gr)

					hdr, err := tr.Next()
					Expect(err).NotTo(HaveOccurred())

					Expect(hdr.Name).To(Equal("./"))

					hdr, err = tr.Next()
					Expect(err).NotTo(HaveOccurred())

					Expect(hdr.Name).To(MatchRegexp("(./)?task.yml$"))
				},
				ghttp.RespondWith(200, ""),
			),
		)
	})

	It("creates a build, streams output, uploads the bits, and polls until completion", func() {
		flyCmd := exec.Command(flyPath, "-t", atcServer.URL(), "e", "-c", taskConfigPath)
		flyCmd.Dir = buildDir

		sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())

		Eventually(streaming).Should(BeClosed())
		Eventually(sess.Out).Should(gbytes.Say("executing build 128"))

		events <- event.Log{Payload: "sup"}

		Eventually(sess.Out).Should(gbytes.Say("sup"))

		close(events)

		<-sess.Exited
		Expect(sess.ExitCode()).To(Equal(0))

		Expect(uploadingBits).To(BeClosed())
	})

	Context("when the build config is invalid", func() {
		BeforeEach(func() {
			// missing platform and run path
			err := ioutil.WriteFile(
				filepath.Join(buildDir, "task.yml"),
				[]byte(`---
run: {}
`),
				0644,
			)
			Expect(err).NotTo(HaveOccurred())
		})

		It("prints the failure and exits 1", func() {
			flyCmd := exec.Command(flyPath, "-t", atcServer.URL(), "e", "-c", taskConfigPath)
			flyCmd.Dir = buildDir

			sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())

			Eventually(sess.Err).Should(gbytes.Say("missing"))

			<-sess.Exited
			Expect(sess.ExitCode()).To(Equal(1))
		})
	})

	Context("when arguments are passed through", func() {
		BeforeEach(func() {
			expectedPlan.OnSuccess.Next.Task.Config.Run.Args = []string{".", "-name", `foo "bar" baz`}
		})

		It("inserts them into the config template", func() {
			atcServer.AllowUnhandledRequests = true

			flyCmd := exec.Command(flyPath, "-t", atcServer.URL(), "e", "-c", taskConfigPath, "--", "-name", "foo \"bar\" baz")
			flyCmd.Dir = buildDir

			sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())

			// sync with after create
			Eventually(streaming, 5.0).Should(BeClosed())

			close(events)

			<-sess.Exited
			Expect(sess.ExitCode()).To(Equal(0))

			Expect(uploadingBits).To(BeClosed())
		})
	})

	Context("when invalid inputs are passed", func() {
		It("prints an error", func() {
			flyCmd := exec.Command(flyPath, "-t", atcServer.URL(), "e", "-c", taskConfigPath, "-i", "fixture=.", "-i", "evan=.")
			flyCmd.Dir = buildDir

			sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())

			Eventually(sess.Err).Should(gbytes.Say("unknown input `evan`"))

			<-sess.Exited
			Expect(sess.ExitCode()).To(Equal(1))
		})

		Context("when invalid inputs are passed and the single valid input is correctly ommited", func() {
			It("prints an error about invalid inputs instead of missing inputs", func() {
				flyCmd := exec.Command(flyPath, "-t", atcServer.URL(), "e", "-c", taskConfigPath, "-i", "evan=.")
				flyCmd.Dir = buildDir

				sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).NotTo(HaveOccurred())

				Eventually(sess.Err).Should(gbytes.Say("unknown input `evan`"))

				<-sess.Exited
				Expect(sess.ExitCode()).To(Equal(1))
			})
		})
	})

	Context("when the task specifies more than one input", func() {

		BeforeEach(func() {
			// missing platform and run path
			err := ioutil.WriteFile(
				filepath.Join(buildDir, "task.yml"),
				[]byte(`---
platform: some-platform

image: ubuntu

inputs:
- name: fixture
- name: something

params:
  FOO: bar
  BAZ: buzz
  X: 1

run:
  path: find
  args: [.]
`),
				0644,
			)
			Expect(err).NotTo(HaveOccurred())
		})
		Context("When some required inputs are not passed", func() {
			It("Prints an error", func() {
				flyCmd := exec.Command(flyPath, "-t", atcServer.URL(), "e", "-c", taskConfigPath, "-i", "something=.")
				flyCmd.Dir = buildDir

				sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).NotTo(HaveOccurred())

				Eventually(sess.Err).Should(gbytes.Say("missing required input `fixture`"))

				<-sess.Exited
				Expect(sess.ExitCode()).To(Equal(1))
			})

		})

		Context("When no inputs are passed", func() {
			It("Prints an error", func() {
				flyCmd := exec.Command(flyPath, "-t", atcServer.URL(), "e", "-c", taskConfigPath)
				flyCmd.Dir = buildDir

				sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).NotTo(HaveOccurred())

				Eventually(sess.Err).Should(gbytes.Say("missing required input"))

				<-sess.Exited
				Expect(sess.ExitCode()).To(Equal(1))
			})

		})

	})

	Context("when running with --privileged", func() {
		BeforeEach(func() {
			expectedPlan.OnSuccess.Next.Task.Privileged = true
		})

		It("inserts them into the config template", func() {
			atcServer.AllowUnhandledRequests = true

			flyCmd := exec.Command(flyPath, "-t", atcServer.URL(), "e", "-c", taskConfigPath, "--privileged")
			flyCmd.Dir = buildDir

			sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())

			// sync with after create
			Eventually(streaming, 5.0).Should(BeClosed())

			close(events)

			<-sess.Exited
			Expect(sess.ExitCode()).To(Equal(0))

			Expect(uploadingBits).To(BeClosed())
		})
	})

	Context("when running with bogus flags", func() {
		It("exits 1", func() {
			flyCmd := exec.Command(flyPath, "-t", atcServer.URL(), "e", "-c", taskConfigPath, "--bogus-flag")
			flyCmd.Dir = buildDir

			sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())

			Eventually(sess.Err).Should(gbytes.Say("unknown flag `bogus-flag'"))

			<-sess.Exited
			Expect(sess.ExitCode()).To(Equal(1))
		})
	})

	Context("when parameters are specified in the environment", func() {
		BeforeEach(func() {
			expectedPlan.OnSuccess.Next.Task.Config.Params = map[string]string{
				"FOO": "newbar",
				"BAZ": "buzz",
				"X":   "",
			}
		})

		It("overrides the build's parameter values", func() {
			atcServer.AllowUnhandledRequests = true

			flyCmd := exec.Command(flyPath, "-t", atcServer.URL(), "e", "-c", taskConfigPath)
			flyCmd.Dir = buildDir
			flyCmd.Env = append(os.Environ(), "FOO=newbar", "X=")

			sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())

			// sync with after create
			Eventually(streaming, 5.0).Should(BeClosed())

			close(events)

			<-sess.Exited
			Expect(sess.ExitCode()).To(Equal(0))

			Expect(uploadingBits).To(BeClosed())
		})
	})

	Context("when the build is interrupted", func() {
		var aborted chan struct{}

		JustBeforeEach(func() {
			aborted = make(chan struct{})

			atcServer.AppendHandlers(
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("POST", "/api/v1/builds/128/abort"),
					func(w http.ResponseWriter, r *http.Request) {
						close(aborted)
					},
				),
			)
		})

		if runtime.GOOS != "windows" {
			Describe("with SIGINT", func() {
				It("aborts the build and exits nonzero", func() {
					flyCmd := exec.Command(flyPath, "-t", atcServer.URL(), "e", "-c", taskConfigPath)
					flyCmd.Dir = buildDir

					sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
					Expect(err).ToNot(HaveOccurred())

					Eventually(streaming, 5).Should(BeClosed())

					Eventually(uploadingBits).Should(BeClosed())

					sess.Signal(os.Interrupt)

					Eventually(aborted, 5.0).Should(BeClosed())

					events <- event.Status{Status: atc.StatusErrored}
					close(events)

					<-sess.Exited
					Expect(sess.ExitCode()).To(Equal(2))
				})
			})

			Describe("with SIGTERM", func() {
				It("aborts the build and exits nonzero", func() {
					flyCmd := exec.Command(flyPath, "-t", atcServer.URL(), "e", "-c", taskConfigPath)
					flyCmd.Dir = buildDir

					sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
					Expect(err).ToNot(HaveOccurred())

					Eventually(streaming, 5).Should(BeClosed())

					Eventually(uploadingBits).Should(BeClosed())

					sess.Signal(syscall.SIGTERM)

					Eventually(aborted, 5.0).Should(BeClosed())

					events <- event.Status{Status: atc.StatusErrored}
					close(events)

					<-sess.Exited
					Expect(sess.ExitCode()).To(Equal(2))
				})
			})
		}
	})

	Context("when the target has an auth token", func() {
		var tmpDir string
		var flyrc string
		var targetName string

		BeforeEach(func() {
			var err error
			tmpDir, err = ioutil.TempDir("", "fly-test")
			Expect(err).NotTo(HaveOccurred())

			if runtime.GOOS == "windows" {
				os.Setenv("USERPROFILE", tmpDir)
			} else {
				os.Setenv("HOME", tmpDir)
			}

			flyrc = filepath.Join(userHomeDir(), ".flyrc")

			targetName = "foo"
			token := rc.TargetToken{
				Type:  "Bearer",
				Value: "some-token",
			}

			err = rc.SaveTarget(
				targetName,
				atcServer.URL(),
				true,
				&token,
			)
			Expect(err).ToNot(HaveOccurred())

			(*expectedPlan.OnSuccess.Step.Aggregate)[0].Get.Source = atc.Source{
				"uri":           atcServer.URL() + "/api/v1/pipes/some-pipe-id",
				"authorization": "Bearer some-token",
			}
		})

		AfterEach(func() {
			os.RemoveAll(tmpDir)
		})

		It("connects with the auth token", func() {
			flyCmd := exec.Command(flyPath, "-t", targetName, "e", "-c", taskConfigPath)
			flyCmd.Dir = buildDir

			sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ToNot(HaveOccurred())

			Eventually(streaming, 5).Should(BeClosed())

			events <- event.Status{Status: atc.StatusSucceeded}
			close(events)

			<-sess.Exited
			Expect(sess.ExitCode()).To(Equal(0))

			Expect(uploadingBits).To(BeClosed())
		})
	})

	Context("when the build succeeds", func() {
		It("exits 0", func() {
			flyCmd := exec.Command(flyPath, "-t", atcServer.URL(), "e", "-c", taskConfigPath)
			flyCmd.Dir = buildDir

			sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ToNot(HaveOccurred())

			Eventually(streaming, 5).Should(BeClosed())

			events <- event.Status{Status: atc.StatusSucceeded}
			close(events)

			<-sess.Exited
			Expect(sess.ExitCode()).To(Equal(0))

			Expect(uploadingBits).To(BeClosed())
		})
	})

	Context("when the build fails", func() {
		It("exits 1", func() {
			flyCmd := exec.Command(flyPath, "-t", atcServer.URL(), "e", "-c", taskConfigPath)
			flyCmd.Dir = buildDir

			sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ToNot(HaveOccurred())

			Eventually(streaming, 5).Should(BeClosed())

			events <- event.Status{Status: atc.StatusFailed}
			close(events)

			<-sess.Exited
			Expect(sess.ExitCode()).To(Equal(1))

			Expect(uploadingBits).To(BeClosed())
		})
	})

	Context("when the build errors", func() {
		It("exits 2", func() {
			flyCmd := exec.Command(flyPath, "-t", atcServer.URL(), "e", "-c", taskConfigPath)
			flyCmd.Dir = buildDir

			sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ToNot(HaveOccurred())

			Eventually(streaming, 5).Should(BeClosed())

			events <- event.Status{Status: atc.StatusErrored}
			close(events)

			<-sess.Exited
			Expect(sess.ExitCode()).To(Equal(2))

			Expect(uploadingBits).To(BeClosed())
		})
	})
})
