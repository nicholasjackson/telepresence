package cli_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/datawire/ambassador/pkg/dtest"
	"github.com/datawire/dlib/dexec"
	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/v2/pkg/client"
	"github.com/telepresenceio/telepresence/v2/pkg/client/cli"
	"github.com/telepresenceio/telepresence/v2/pkg/version"
)

// serviceCount is the number of interceptable services that gets installed
// in the cluster and later intercepted
const serviceCount = 3

func TestTelepresence(t *testing.T) {
	dtest.WithMachineLock(func() {
		suite.Run(t, new(telepresenceSuite))
	})
}

type telepresenceSuite struct {
	suite.Suite
	testVersion          string
	namespace            string
	managerTestNamespace string
}

func (ts *telepresenceSuite) SetupSuite() {
	// Check that the "ko" program exists, and adjust PATH as necessary.
	if info, err := os.Stat("../../../tools/bin/ko"); err != nil || !info.Mode().IsRegular() || (info.Mode().Perm()&0100) == 0 {
		ts.FailNow("it looks like the ./tools/bin/ko executable wasn't built; be sure to build it with `make` before running `go test`!")
	}
	require := ts.Require()
	toolbindir, err := filepath.Abs("../../../tools/bin")
	require.NoError(err)
	_ = os.Chdir("../../..")

	os.Setenv("PATH", toolbindir+":"+os.Getenv("PATH"))

	// Remove very verbose output from DTEST initialization
	log.SetOutput(ioutil.Discard)

	ts.testVersion = fmt.Sprintf("v2.0.0-gotest.%d", os.Getpid())
	ts.namespace = fmt.Sprintf("telepresence-%d", os.Getpid())
	ts.managerTestNamespace = fmt.Sprintf("ambassador-%d", os.Getpid())

	version.Version = ts.testVersion

	ctx := dlog.NewTestContext(ts.T(), false)

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		executable, err := ts.buildExecutable(ctx)
		ts.NoError(err)
		client.SetExe(executable)
	}()

	_ = os.Remove(client.ConnectorSocketName)
	err = run(ctx, "sudo", "true")
	require.NoError(err, "acquire privileges")

	registry := dtest.DockerRegistry()
	os.Setenv("KO_DOCKER_REPO", registry)
	os.Setenv("TELEPRESENCE_REGISTRY", registry)
	os.Setenv("TELEPRESENCE_MANAGER_NAMESPACE", ts.managerTestNamespace)

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := ts.publishManager()
		ts.NoError(err)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()

		kubeconfig := dtest.Kubeconfig()
		os.Setenv("DTEST_KUBECONFIG", kubeconfig)
		os.Setenv("KUBECONFIG", kubeconfig)
		err = run(ctx, "kubectl", "create", "namespace", ts.namespace)
		ts.NoError(err)
		err = run(ctx, "kubectl", "apply", "-f", "k8s/client_rbac.yaml")
		ts.NoError(err)

		// This is how we create a user that has their rbac restricted to what we have in
		// k8s/client_rbac.yaml. We do this by creating a service account and then getting
		// the token from said service account and storing it in our kubeconfig.
		secret, err := output(ctx, "kubectl", "get", "sa", "telepresence-test-developer", "-o", "jsonpath={.secrets[0].name}")
		ts.NoError(err)
		encSecret, err := output(ctx, "kubectl", "get", "secret", secret, "-o", "jsonpath={.data.token}")
		ts.NoError(err)
		token, err := base64.StdEncoding.DecodeString(encSecret)
		ts.NoError(err)
		err = run(ctx, "kubectl", "config", "set-credentials", "telepresence-test-developer", "--token", string(token))
		ts.NoError(err)
		err = run(ctx, "kubectl", "config", "set-context", "telepresence-test-developer", "--user", "telepresence-test-developer", "--cluster", "default")
		ts.NoError(err)

		// We start with the default context, and will switch to the
		// telepresence-test-developer user later in the tests
		err = run(ctx, "kubectl", "config", "use-context", "default")
		ts.NoError(err)
	}()
	wg.Wait()

	wg.Add(serviceCount)
	for i := 0; i < serviceCount; i++ {
		i := i
		go func() {
			defer wg.Done()
			err = ts.applyEchoService(ctx, fmt.Sprintf("hello-%d", i))
			ts.NoError(err)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		err = ts.applyApp(ctx, "with-probes", "with-probes", 80)
		ts.NoError(err)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		err = ts.applyApp(ctx, "rs-echo", "rs-echo", 80)
		ts.NoError(err)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		err = ts.applyApp(ctx, "ss-echo", "ss-echo", 80)
		ts.NoError(err)
	}()
	wg.Wait()

	// Ensure that telepresence is not logged in
	_, _ = telepresence(ts.T(), "logout")

	// Ensure that no telepresence is running when the tests start
	_, _ = telepresence(ts.T(), "quit")
}

func (ts *telepresenceSuite) TearDownSuite() {
	ctx := dlog.NewTestContext(ts.T(), false)
	_ = run(ctx, "kubectl", "config", "use-context", "default")
	_ = run(ctx, "kubectl", "delete", "namespace", ts.namespace)
	_ = run(ctx, "kubectl", "delete", "namespace", ts.managerTestNamespace)
	// Undo RBAC things
	_ = run(ctx, "kubectl", "delete", "-f", "k8s/client_rbac.yaml")
	_ = run(ctx, "kubectl", "config", "delete-context", "telepresence-test-developer")
	_ = run(ctx, "kubectl", "config", "delete-user", "telepresence-test-developer")
}

func (ts *telepresenceSuite) TestA_WithNoDaemonRunning() {
	ts.Run("Version", func() {
		stdout, stderr := telepresence(ts.T(), "version")
		ts.Empty(stderr)
		ts.Contains(stdout, fmt.Sprintf("Client %s", client.DisplayVersion()))
	})
	ts.Run("Status", func() {
		out, _ := telepresence(ts.T(), "status")
		ts.Contains(out, "Root Daemon: Not running")
		ts.Contains(out, "User Daemon: Not running")
	})

	ts.Run("Connect using invalid KUBECONFIG", func() {
		ts.Run("Reports config error and exits", func() {
			kubeConfig := os.Getenv("KUBECONFIG")
			defer os.Setenv("KUBECONFIG", kubeConfig)
			os.Setenv("KUBECONFIG", "/dev/null")
			stdout, stderr := telepresence(ts.T(), "connect")
			ts.Contains(stderr, "kubeconfig has no context definition")
			ts.Contains(stdout, "Launching Telepresence Daemon")
			ts.Contains(stdout, "Daemon quitting")
		})
	})

	ts.Run("Connect with non existing context", func() {
		ts.Run("Reports connect error and exits", func() {
			stdout, stderr := telepresence(ts.T(), "connect", "--context", "not-likely-to-exist")
			ts.Contains(stderr, `"not-likely-to-exist" does not exist`)
			ts.Contains(stdout, "Launching Telepresence Daemon")
			ts.Contains(stdout, "Daemon quitting")
		})
	})

	ts.Run("Connect with a command", func() {
		ts.Run("Connects, executes the command, and then exits", func() {
			stdout, stderr := telepresence(ts.T(), "connect", "--", client.GetExe(), "status")
			require := ts.Require()
			require.Empty(stderr)
			require.Contains(stdout, "Launching Telepresence Daemon")
			require.Contains(stdout, "Connected to context")
			require.Contains(stdout, "Kubernetes context:")
			require.Regexp(`Telepresence proxy:\s+ON`, stdout)
			require.Contains(stdout, "Daemon quitting")
		})
	})
}

func (ts *telepresenceSuite) TestB_Connected() {
	suite.Run(ts.T(), &connectedSuite{tpSuite: ts})
}

func (ts *telepresenceSuite) TestC_Uninstall() {
	ts.Run("Uninstalls the traffic manager and quits", func() {
		require := ts.Require()
		ctx := dlog.NewTestContext(ts.T(), false)
		names := func() (string, error) {
			return ts.kubectlOut(ctx, "get",
				"--namespace", ts.managerTestNamespace,
				"svc,deploy", "traffic-manager",
				"--ignore-not-found",
				"-o", "jsonpath={.items[*].metadata.name}")
		}
		stdout, err := names()
		require.NoError(err)
		require.Equal(2, len(strings.Split(stdout, " "))) // The service and the deployment
		stdout, stderr := telepresence(ts.T(), "uninstall", "--everything")
		require.Empty(stderr)
		require.Contains(stdout, "Daemon quitting")
		require.Eventually(
			func() bool {
				stdout, _ := names()
				return stdout == ""
			},
			5*time.Second,        // waitFor
			500*time.Millisecond, // polling interval
		)
	})
}

type connectedSuite struct {
	suite.Suite
	tpSuite *telepresenceSuite
}

func (cs *connectedSuite) ns() string {
	return cs.tpSuite.namespace
}

func (cs *connectedSuite) SetupSuite() {
	require := cs.Require()
	c := dlog.NewTestContext(cs.T(), false)
	cs.NoError(cs.tpSuite.kubectl(c, "config", "use-context", "telepresence-test-developer"))
	stdout, stderr := telepresence(cs.T(), "connect")
	require.Empty(stderr)
	require.Contains(stdout, "Connected to context")

	// Give outbound interceptor 15 seconds to kick in.
	require.Eventually(
		// condition
		func() bool {
			stdout, _ := telepresence(cs.T(), "status")
			return regexp.MustCompile(`Telepresence proxy:\s+ON`).FindString(stdout) != ""
		},
		15*time.Second, // waitFor
		time.Second,    // polling interval
		"Timeout waiting for network overrides to establish", // msg
	)
}

func (cs *connectedSuite) TearDownSuite() {
	stdout, stderr := telepresence(cs.T(), "quit")
	cs.Empty(stderr)
	cs.Contains(stdout, "quitting")
	c := dlog.NewTestContext(cs.T(), false)
	cs.NoError(cs.tpSuite.kubectl(c, "config", "use-context", "default"))
	time.Sleep(time.Second) // Allow some time for processes to die and sockets to vanish
}

func (cs *connectedSuite) TestA_ReportsVersionFromDaemon() {
	stdout, stderr := telepresence(cs.T(), "version")
	cs.Empty(stderr)
	vs := client.DisplayVersion()
	cs.Contains(stdout, fmt.Sprintf("Client %s", vs))
	cs.Contains(stdout, fmt.Sprintf("Daemon %s", vs))
}

func (cs *connectedSuite) TestB_ReportsStatusAsConnected() {
	stdout, stderr := telepresence(cs.T(), "status")
	cs.Empty(stderr)
	cs.Contains(stdout, "Kubernetes context:")
}

func (cs *connectedSuite) TestC_ProxiesOutboundTraffic() {
	for i := 0; i < serviceCount; i++ {
		svc := fmt.Sprintf("hello-%d.%s", i, cs.ns())
		expectedOutput := fmt.Sprintf("Request served by hello-%d", i)
		cs.Require().Eventually(
			// condition
			func() bool {
				cs.T().Logf("trying %q...", "http://"+svc)
				resp, err := http.Get("http://" + svc)
				if err != nil {
					cs.T().Log(err)
					return false
				}
				defer resp.Body.Close()
				cs.T().Logf("status code: %v", resp.StatusCode)
				body, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					cs.T().Log(err)
					return false
				}
				cs.T().Logf("body: %q", body)
				return strings.Contains(string(body), expectedOutput)
			},
			15*time.Second, // waitfor
			3*time.Second,  // polling interval
			`body of %q contains %q`, "http://"+svc, expectedOutput,
		)
	}
}

func (cs *connectedSuite) TestD_Intercepted() {
	suite.Run(cs.T(), &interceptedSuite{tpSuite: cs.tpSuite})
}

func (cs *connectedSuite) TestE_PodWithSubdomain() {
	require := cs.Require()
	c := dlog.NewTestContext(cs.T(), false)
	require.NoError(cs.tpSuite.applyApp(c, "echo-w-subdomain", "echo.subsonic", 8080))
	defer func() {
		cs.NoError(cs.tpSuite.kubectl(c, "delete", "svc", "subsonic", "--context", "default"))
		cs.NoError(cs.tpSuite.kubectl(c, "delete", "deploy", "echo-subsonic", "--context", "default"))
	}()

	cc, cancel := context.WithTimeout(c, 3*time.Second)
	defer cancel()
	ip, err := net.DefaultResolver.LookupHost(cc, "echo.subsonic."+cs.ns())
	cs.NoError(err)
	cs.True(len(ip) == 1)
	ip, err = net.DefaultResolver.LookupHost(cc, "echo.subsonic."+cs.ns()+".svc.cluster.local")
	cs.NoError(err)
	cs.True(len(ip) == 1)
}

func (cs *connectedSuite) TestF_SuccessfullyInterceptsDeploymentWithProbes() {
	defer telepresence(cs.T(), "leave", "with-probes-"+cs.ns())

	require := cs.Require()
	stdout, stderr := telepresence(cs.T(), "intercept", "--namespace", cs.ns(), "--mount", "false", "with-probes", "--port", "9090")
	require.Empty(stderr)
	require.Contains(stdout, "Using Deployment with-probes")
	stdout, stderr = telepresence(cs.T(), "list", "--namespace", cs.ns(), "--intercepts")
	require.Empty(stderr)
	require.Contains(stdout, "with-probes: intercepted")
}

func (cs *connectedSuite) TestG_SuccessfullyInterceptsReplicaSet() {
	defer telepresence(cs.T(), "leave", "rs-echo-"+cs.ns())

	require := cs.Require()
	stdout, stderr := telepresence(cs.T(), "intercept", "--namespace", cs.ns(), "--mount", "false", "rs-echo", "--port", "9091")
	require.Empty(stderr)
	require.Contains(stdout, "Using ReplicaSet rs-echo")
	stdout, stderr = telepresence(cs.T(), "list", "--namespace", cs.ns(), "--intercepts")
	require.Empty(stderr)
	require.Contains(stdout, "rs-echo: intercepted")
}

func (cs *connectedSuite) TestH_SuccessfullyInterceptsStatefulSet() {
	defer telepresence(cs.T(), "leave", "ss-echo-"+cs.ns())

	require := cs.Require()
	stdout, stderr := telepresence(cs.T(), "intercept", "--namespace", cs.ns(), "--mount", "false", "ss-echo", "--port", "9091")
	require.Empty(stderr)
	require.Contains(stdout, "Using StatefulSet ss-echo")
	stdout, stderr = telepresence(cs.T(), "list", "--namespace", cs.ns(), "--intercepts")
	require.Empty(stderr)
	require.Contains(stdout, "ss-echo: intercepted")
}

func (cs *connectedSuite) TestI_LocalOnlyIntercept() {
	cs.Run("intercept can be established", func() {
		stdout, stderr := telepresence(cs.T(), "intercept", "--namespace", cs.ns(), "--local-only", "mylocal")
		cs.Empty(stdout)
		cs.Empty(stderr)
	})

	cs.Run("is included in list output", func() {
		// list includes local intercept
		stdout, stderr := telepresence(cs.T(), "list", "--namespace", cs.ns(), "--intercepts")
		cs.Empty(stderr)
		cs.Contains(stdout, "mylocal: local-only intercept")
	})

	cs.Run("makes services reachable using unqualified name", func() {
		ctx := dlog.NewTestContext(cs.T(), false)

		// service can be resolve with unqualified name
		ip, err := net.DefaultResolver.LookupHost(ctx, "hello-0")
		cs.NoError(err)
		cs.True(len(ip) == 1)
	})

	cs.Run("leaving renders services unavailable using unqualified name", func() {
		stdout, stderr := telepresence(cs.T(), "leave", "mylocal")
		cs.Empty(stdout)
		cs.Empty(stderr)
		ctx := dlog.NewTestContext(cs.T(), false)
		cs.Eventually(func() bool {
			_, err := net.DefaultResolver.LookupHost(ctx, "hello-0")
			return err != nil
		}, 3*time.Second, 300*time.Millisecond)
	})
}

func (cs *connectedSuite) TestJ_ListOnlyMapped() {
	require := cs.Require()
	stdout, stderr := telepresence(cs.T(), "connect", "--mapped-namespaces", "default")
	require.Empty(stderr)
	require.Empty(stdout)

	stdout, stderr = telepresence(cs.T(), "list", "--namespace", cs.ns())
	require.Empty(stderr)
	require.Contains(stdout, "No Workloads (Deployments, StatefulSets, or ReplicaSets)")

	stdout, stderr = telepresence(cs.T(), "connect", "--mapped-namespaces", "all")
	require.Empty(stderr)
	require.Empty(stdout)

	stdout, stderr = telepresence(cs.T(), "list", "--namespace", cs.ns())
	require.Empty(stderr)
	require.NotContains(stdout, "No Workloads (Deployments, StatefulSets, or ReplicaSets)")
}

func (cs *connectedSuite) TestK_Uninstall() {
	cs.Run("Uninstalls agent on given deployment", func() {
		require := cs.Require()
		stdout, stderr := telepresence(cs.T(), "list", "--namespace", cs.ns(), "--agents")
		require.Empty(stderr)
		require.Contains(stdout, "with-probes")
		_, stderr = telepresence(cs.T(), "uninstall", "--namespace", cs.ns(), "--agent", "with-probes")
		require.Empty(stderr)
		require.Eventually(
			// condition
			func() bool {
				stdout, _ := telepresence(cs.T(), "list", "--namespace", cs.ns(), "--agents")
				return !strings.Contains(stdout, "with-probes")
			},
			30*time.Second, // waitFor
			2*time.Second,  // polling interval
		)
	})

	cs.Run("Uninstalls agent on given replicaset", func() {
		require := cs.Require()
		stdout, stderr := telepresence(cs.T(), "list", "--namespace", cs.ns(), "--agents")
		require.Empty(stderr)
		require.Contains(stdout, "rs-echo")
		_, stderr = telepresence(cs.T(), "uninstall", "--namespace", cs.ns(), "--agent", "rs-echo")
		require.Empty(stderr)
		require.Eventually(
			// condition
			func() bool {
				stdout, _ := telepresence(cs.T(), "list", "--namespace", cs.ns(), "--agents")
				return !strings.Contains(stdout, "rs-echo")
			},
			30*time.Second, // waitFor
			2*time.Second,  // polling interval
		)
	})

	cs.Run("Uninstalls agent on given statefulset", func() {
		require := cs.Require()
		stdout, stderr := telepresence(cs.T(), "list", "--namespace", cs.ns(), "--agents")
		require.Empty(stderr)
		require.Contains(stdout, "ss-echo")
		_, stderr = telepresence(cs.T(), "uninstall", "--namespace", cs.ns(), "--agent", "ss-echo")
		require.Empty(stderr)
		require.Eventually(
			// condition
			func() bool {
				stdout, _ := telepresence(cs.T(), "list", "--namespace", cs.ns(), "--agents")
				return !strings.Contains(stdout, "ss-echo")
			},
			30*time.Second, // waitFor
			2*time.Second,  // polling interval
		)
	})

	cs.Run("Uninstalls all agents", func() {
		require := cs.Require()
		stdout, stderr := telepresence(cs.T(), "list", "--namespace", cs.ns(), "--agents")
		require.Empty(stderr)
		require.GreaterOrEqual(len(strings.Split(stdout, "\n")), serviceCount)
		_, stderr = telepresence(cs.T(), "uninstall", "--namespace", cs.ns(), "--all-agents")
		require.Empty(stderr)
		require.Eventually(
			func() bool {
				stdout, _ := telepresence(cs.T(), "list", "--namespace", cs.ns(), "--agents")
				return stdout == "No Workloads (Deployments, StatefulSets, or ReplicaSets)"
			},
			30*time.Second,     // waitFor
			2*time.Millisecond, // polling interval
		)
	})
}

type interceptedSuite struct {
	suite.Suite
	tpSuite    *telepresenceSuite
	intercepts []string
	services   []*http.Server
}

func (is *interceptedSuite) ns() string {
	return is.tpSuite.namespace
}

func (is *interceptedSuite) SetupSuite() {
	is.intercepts = make([]string, 0, serviceCount)
	is.services = make([]*http.Server, 0, serviceCount)

	is.Run("all intercepts ready", func() {
		rxs := make([]*regexp.Regexp, serviceCount)
		for i := 0; i < serviceCount; i++ {
			rxs[i] = regexp.MustCompile(fmt.Sprintf("hello-%d\\s*:\\s+ready to intercept", i))
		}
		is.Require().Eventually(
			// condition
			func() bool {
				stdout, _ := telepresence(is.T(), "list", "--namespace", is.ns())
				is.T().Log(stdout)
				for i := 0; i < serviceCount; i++ {
					if !rxs[i].MatchString(stdout) {
						return false
					}
				}
				return true
			},
			15*time.Second, // waitFor
			3*time.Second,  // polling interval
			`telepresence list reports all agents`,
		)
	})

	is.Run("adding intercepts", func() {
		for i := 0; i < serviceCount; i++ {
			svc := fmt.Sprintf("hello-%d", i)
			port := strconv.Itoa(9000 + i)
			stdout, stderr := telepresence(is.T(), "intercept", "--namespace", is.ns(), "--mount", "false", svc, "--port", port)
			is.Require().Empty(stderr)
			is.intercepts = append(is.intercepts, svc)
			is.Contains(stdout, "Using Deployment "+svc)
		}
	})

	is.Run("starting http servers", func() {
		for i := 0; i < serviceCount; i++ {
			svc := fmt.Sprintf("hello-%d", i)
			port := strconv.Itoa(9000 + i)
			srv := &http.Server{Addr: ":" + port, Handler: http.NewServeMux()}
			go func() {
				srv.Handler.(*http.ServeMux).HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprintf(w, "%s from intercept at %s", svc, r.URL.Path)
				})
				is.services = append(is.services, srv)
				err := srv.ListenAndServe()
				is.Equal(http.ErrServerClosed, err)
			}()
		}
	})
}

func (is *interceptedSuite) TearDownSuite() {
	for _, svc := range is.intercepts {
		stdout, stderr := telepresence(is.T(), "leave", svc+"-"+is.ns())
		is.Empty(stderr)
		is.Empty(stdout)
	}
	for _, srv := range is.services {
		_ = srv.Shutdown(context.Background())
	}
	time.Sleep(time.Second) // Allow some time for processes to die and intercepts to vanish
}

func (is *interceptedSuite) TestA_VerifyingResponsesFromInterceptor() {
	for i := 0; i < serviceCount; i++ {
		svc := fmt.Sprintf("hello-%d", i)
		expectedOutput := fmt.Sprintf("%s from intercept at /", svc)
		is.Require().Eventually(
			// condition
			func() bool {
				is.T().Logf("trying %q...", "http://"+svc)
				resp, err := http.Get("http://" + svc)
				if err != nil {
					is.T().Log(err)
					return false
				}
				defer resp.Body.Close()
				is.T().Logf("status code: %v", resp.StatusCode)
				body, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					is.T().Log(err)
					return false
				}
				is.T().Logf("body: %q", body)
				return string(body) == expectedOutput
			},
			15*time.Second, // waitFor
			3*time.Second,  // polling interval
			`body of %q equals %q`, "http://"+svc, expectedOutput,
		)
	}
}

func (is *interceptedSuite) TestB_ListingActiveIntercepts() {
	require := is.Require()
	stdout, stderr := telepresence(is.T(), "--namespace", is.ns(), "list", "--intercepts")
	require.Empty(stderr)
	for i := 0; i < serviceCount; i++ {
		require.Contains(stdout, fmt.Sprintf("hello-%d: intercepted", i))
	}
}

func (ts *telepresenceSuite) applyApp(c context.Context, name, svcName string, port int) error {
	err := ts.kubectl(c, "apply", "-f", fmt.Sprintf("k8s/%s.yaml", name), "--context", "default")
	if err != nil {
		return fmt.Errorf("failed to deploy %s: %v", name, err)
	}
	return ts.waitForService(c, svcName, port)
}

func (ts *telepresenceSuite) applyEchoService(c context.Context, name string) error {
	err := ts.kubectl(c, "create", "deploy", name, "--image", "jmalloc/echo-server:0.1.0")
	if err != nil {
		return fmt.Errorf("failed to create deployment %s: %v", name, err)
	}
	err = ts.kubectl(c, "expose", "deploy", name, "--port", "80", "--target-port", "8080")
	if err != nil {
		return fmt.Errorf("failed to expose deployment %s: %v", name, err)
	}
	return ts.waitForService(c, name, 80)
}

func (ts *telepresenceSuite) waitForService(c context.Context, name string, port int) error {
	c, cancel := context.WithTimeout(c, 60*time.Second)
	defer cancel()

	// Since this function can be called multiple times in parallel
	// we add the name of the servie to the title of the pod so they
	// can run at the same time. We strip out any characters that we
	// can't use in a name in k8s.
	reg := regexp.MustCompile("[^a-zA-Z0-9-]+")
	k8sSafeName := reg.ReplaceAllString(name, "")
	containerName := fmt.Sprintf("curl-%s-from-cluster", k8sSafeName)
	for i := 0; i < 60; i++ {
		time.Sleep(time.Second)
		err := ts.kubectl(c, "run", containerName, "--context", "default", "--rm", "-it",
			"--image=docker.io/pstauffer/curl", "--restart=Never", "--",
			"curl", "--silent", "--output", "/dev/null",
			fmt.Sprintf("http://%s.%s:%d", name, ts.namespace, port),
		)
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("timed out waiting for %s service", name)
}

func (ts *telepresenceSuite) kubectl(c context.Context, args ...string) error {
	return run(c, append([]string{"kubectl", "--namespace", ts.namespace}, args...)...)
}

func (ts *telepresenceSuite) kubectlOut(ctx context.Context, args ...string) (string, error) {
	return output(ctx, append([]string{"kubectl", "--namespace", ts.namespace}, args...)...)
}

func (ts *telepresenceSuite) publishManager() error {
	ctx := dlog.NewTestContext(ts.T(), true)
	cmd := dexec.CommandContext(ctx, "make", "push-image")

	// Go sets a lot of variables that we don't want to pass on to the ko executable. If we do,
	// then it builds for the platform indicated by those variables.
	cmd.Env = []string{
		"TELEPRESENCE_VERSION=" + ts.testVersion,
		"TELEPRESENCE_REGISTRY=" + dtest.DockerRegistry(),
	}
	includeEnv := []string{"KO_DOCKER_REPO=", "HOME=", "PATH=", "LOGNAME=", "TMPDIR=", "MAKELEVEL="}
	for _, env := range os.Environ() {
		for _, incl := range includeEnv {
			if strings.HasPrefix(env, incl) {
				cmd.Env = append(cmd.Env, env)
				break
			}
		}
	}
	if err := cmd.Run(); err != nil {
		return client.RunError(err)
	}
	return nil
}

func (ts *telepresenceSuite) buildExecutable(c context.Context) (string, error) {
	executable := filepath.Join("build-output", "bin", "/telepresence")
	return executable, run(c, "go", "build", "-ldflags",
		fmt.Sprintf("-X=github.com/telepresenceio/telepresence/v2/pkg/version.Version=%s", ts.testVersion),
		"-o", executable, "./cmd/telepresence")
}

func run(c context.Context, args ...string) error {
	return client.RunError(dexec.CommandContext(c, args[0], args[1:]...).Run())
}

func output(ctx context.Context, args ...string) (string, error) {
	cmd := dexec.CommandContext(ctx, args[0], args[1:]...)
	cmd.DisableLogging = true
	out, err := cmd.Output()
	return string(out), client.RunError(err)
}

// telepresence executes the CLI command in-process
func telepresence(t testing.TB, args ...string) (string, string) {
	ctx := dlog.NewTestContext(t, false)
	dlog.Infof(ctx, "running command: %q", append([]string{"telepresence"}, args...))

	cmd := cli.Command(ctx)

	stdout := new(strings.Builder)
	cmd.SetOut(io.MultiWriter(
		stdout,
		dlog.StdLogger(dlog.WithField(ctx, "stream", "stdout"), dlog.LogLevelInfo).Writer(),
	))

	stderr := new(strings.Builder)
	cmd.SetErr(io.MultiWriter(
		stderr,
		dlog.StdLogger(dlog.WithField(ctx, "stream", "stderr"), dlog.LogLevelInfo).Writer(),
	))

	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), err)
	}

	dlog.Infof(ctx, "command terminated %q", append([]string{"telepresence"}, args...))
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String())
}
