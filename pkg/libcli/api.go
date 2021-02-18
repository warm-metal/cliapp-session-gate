package libcli

import (
	"bufio"
	"context"
	"fmt"
	"github.com/moby/term"
	"github.com/warm-metal/cliapp-session-gate/pkg/rpc"
	appcorev1 "github.com/warm-metal/cliapp/pkg/apis/cliapp/v1"
	"golang.org/x/sys/unix"
	"golang.org/x/xerrors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"io"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/exec"
	"k8s.io/klog/v2"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"time"
)

func FetchGateEndpoints(clientset *kubernetes.Clientset) ([]string, error)  {
	return fetchServiceEndpoints(clientset, "cliapp-system", "cliapp-session-gate", "session-gate")
}

func fetchServiceEndpoints(clientset *kubernetes.Clientset, namespace, service, port string) (addrs []string, err error) {
	svc, err := clientset.CoreV1().Services(namespace).
		Get(context.TODO(), service, metav1.GetOptions{})
	if err != nil {
		return nil, xerrors.Errorf(
			`can't fetch endpoint from Service "%s/%s": %s`, namespace, service, err)
	}

	svcPort := int32(0)
	nodePort := int32(0)
	for _, p := range svc.Spec.Ports {
		if p.Name != port {
			continue
		}

		svcPort = p.Port
		nodePort = p.NodePort
	}

	if svcPort > 0 {
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
			if len(ingress.Hostname) > 0 {
				// FIXME deduct scheme from port protocol
				addrs = append(addrs, fmt.Sprintf("tcp://%s:%d", ingress.Hostname, svcPort))
			}

			if len(ingress.IP) > 0 {
				addrs = append(addrs, fmt.Sprintf("tcp://%s:%d", ingress.IP, svcPort))
			}
		}
	}

	if nodePort > 0 {
		nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return nil, xerrors.Errorf(`can't list node while enumerating Service NodePort: %s`, err)
		}

		for _, node := range nodes.Items {
			for _, addr := range node.Status.Addresses {
				if len(addr.Address) > 0 {
					addrs = append(addrs, fmt.Sprintf("tcp://%s:%d", addr.Address, nodePort))
				}
			}
		}
	}

	addrs = append(addrs, fmt.Sprintf("tcp://%s:%d", svc.Spec.ClusterIP, svcPort))
	return
}

func ExecCliApp(endpoints []string, app *appcorev1.CliApp, args []string, stdin io.Reader, stdout io.Writer) error {
	stdInFd, isTerminal := term.GetFdInfo(stdin)
	if !isTerminal {
		return xerrors.Errorf("can't execute the command without a terminal")
	}

	stdOutFd, isTerminal := term.GetFdInfo(stdout)
	if  !isTerminal {
		return xerrors.Errorf("can't execute the command without a terminal")
	}

	var cc *grpc.ClientConn
	for i, ep := range endpoints {
		endpoint, err := url.Parse(ep)
		if err != nil {
			panic(err)
		}
		ctx, cancel := context.WithTimeout(context.TODO(), 3*time.Second)
		cc, err = grpc.DialContext(ctx, endpoint.Host, grpc.WithInsecure(), grpc.WithBlock())
		cancel()
		if err == nil {
			break
		}

		fmt.Fprintf(os.Stderr, `can't connect to app session gate "%s": %s`+"\n", endpoint.Host, err)
		i++
		if i < len(endpoints) {
			fmt.Fprintf(os.Stderr, `Try the next endpoint %s`+"\n", endpoints[i])
		}
	}

	if cc == nil {
		return xerrors.Errorf("all remote endpoints are unavailable")
	}

	appCli := rpc.NewAppGateClient(cc)

	sh, err := appCli.OpenShell(context.TODO())
	if err != nil {
		return xerrors.Errorf("can't open app session: %s", err)
	}

	err = sh.Send(&rpc.StdIn{
		App: &rpc.App{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
		Input:        args,
		TerminalSize: getSize(stdOutFd),
	})

	if err != nil {
		return xerrors.Errorf("can't open app: %s", err)
	}

	errCh := make(chan error)
	defer close(errCh)

	inCh := make(chan string)

	go func() {
		stdReader := bufio.NewReader(stdin)
		defer close(inCh)
		for {
			line, prefix, err := stdReader.ReadLine()
			if err != nil && err != io.EOF {
				errCh <- xerrors.Errorf("can't read the input:%s", err)
				return
			}

			if err == io.EOF {
				return
			}

			if prefix {
				errCh <- xerrors.Errorf("line is too lang")
				return
			}

			inCh <- string(line)
		}
	}()

	outCh := make(chan string)
	go func() {
		defer close(outCh)
		for {
			resp, err := sh.Recv()
			if err != nil {
				if err == io.EOF {
					errCh <- err
					return
				}

				st, ok := status.FromError(err)
				if ok && st.Code() == codes.Aborted {
					if code, failed := strconv.Atoi(st.Message()); failed == nil {
						errCh <- exec.CodeExitError{Code: code, Err: err}
						return
					}
				}

				errCh <- xerrors.Errorf("can't read the remote response:%s", err)
				return
			}

			if len(resp.Output) > 0 {
				outCh <- resp.Output
			}
		}
	}()

	state, err := term.MakeRaw(stdInFd)
	if err != nil {
		return xerrors.Errorf("can't initialize terminal: %s", err)
	}

	defer func() {
		term.RestoreTerminal(stdInFd, state)
	}()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, unix.SIGWINCH)
	defer signal.Stop(winch)

	for {
		select {
		case err := <-errCh:
			if err == io.EOF {
				return nil
			}

			return err
		case <-winch:
			size := getSize(stdOutFd)
			if err = sh.Send(&rpc.StdIn{TerminalSize: size}); err != nil {
				return err
			}
		case in, ok := <-inCh:
			if ok {
				if err = sh.Send(&rpc.StdIn{Input: []string{in}}); err != nil {
					return err
				}
			}
		case out, ok := <-outCh:
			if ok {
				fmt.Fprint(stdout, out)
			}
		}
	}

	return nil
}

func getSize(fd uintptr) *rpc.TerminalSize {
	winsize, err := term.GetWinsize(fd)
	if err != nil {
		klog.Errorf("unable to get terminal size: %v", err)
		return nil
	}

	return &rpc.TerminalSize{Width: uint32(winsize.Width), Height: uint32(winsize.Height)}
}
