package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/omrikiei/ktunnel/pkg/client"
	"github.com/omrikiei/ktunnel/pkg/k8s"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var Reuse bool
var NodeSelectorTags []string

var exposeCmd = &cobra.Command{
	Use:   "expose [flags] SERVICE_NAME [ports]",
	Short: "Expose local machine as a service on the kubernetes cluster",
	Long: `This command would inject a new service and deployment to the cluster, and open the tunnel to the server 
			forwarding tunnel ingress traffic to the the same port on localhost`,
	Args: cobra.MinimumNArgs(2),
	Example: `
# Expose a local application running on port 8000 via http
ktunnel expose kewlapp 80:8000

ktunnel expose kewlapp 80:8000 -r
			  
# Expose a local redis server
ktunnel expose redis 6379
              `,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithCancel(context.Background())
		if verbose {
			logger.SetLevel(log.DebugLevel)
			k8s.Verbose = true
		}
		o := sync.Once{}

		// Create service and deployment
		svcName, ports := args[0], args[1:]
		readyChan := make(chan bool, 1)
		nodeSelectorTags := map[string]string{}
		for _, tag := range NodeSelectorTags {
			parsed := strings.Split(tag, "=")
			log.Error(parsed)
			if len(parsed) != 2 {
				log.Errorf("failed to parse node selector tag: %v", tag)
			} else {
				nodeSelectorTags[parsed[0]] = parsed[1]
			}
		}
		err := k8s.ExposeAsService(&Namespace, &svcName, port, Scheme, ports, ServerImage, Reuse, readyChan, nodeSelectorTags)
		if err != nil {
			log.Fatalf("Failed to expose local machine as a service: %v", err)
		}
		sigs := make(chan os.Signal, 1)
		wg := &sync.WaitGroup{}
		done := make(chan bool, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL, syscall.SIGQUIT)

		// Teardown
		go func() {
			o.Do(func() {
				_ = <-sigs
				if Reuse {
					log.Info("Got exit signal, closing client tunnels")
				} else {
					log.Info("Got exit signal, closing client tunnels and removing k8s objects")
				}
				cancel()
				if !Reuse {
					err := k8s.TeardownExposedService(Namespace, svcName)
					if err != nil {
						log.Errorf("Failed deleting k8s objects: %s", err)
					}
				}
				done <- true
			})
		}()

		log.Info("waiting for deployment to be ready")
		<-readyChan

		// port-Forward
		strPort := strconv.FormatInt(int64(port), 10)
		stopChan := make(chan struct{}, 1)
		// Create a tunnel client for each replica
		sourcePorts, err := k8s.PortForward(&Namespace, &svcName, strPort, wg, stopChan)
		if err != nil {
			log.Fatalf("Failed to run port forwarding: %v", err)
			os.Exit(1)
		}
		log.Info("Waiting for port forward to finish")
		wg.Wait()
		for _, srcPort := range *sourcePorts {
			go func() {
				p, err := strconv.ParseInt(srcPort, 10, 0)
				if err != nil {
					log.Fatalf("Failed to run client: %v", err)
				}
				prt := int(p)
				opts := []client.ClientOption{
					client.WithServer(Host, prt),
					client.WithTunnels(Scheme, ports...),
					client.WithLogger(&logger),
				}
				if tls {
					opts = append(opts, client.WithTLS(CertFile, ServerHostOverride))
				}
				err = client.RunClient(ctx, opts...)
				if err != nil {
					log.Fatalf("Failed to run client: %v", err)
				}
			}()
		}
		_ = <-done
	},
}

func init() {
	exposeCmd.Flags().StringVarP(&CaFile, "ca-file", "c", "", "TLS cert auth file")
	exposeCmd.Flags().StringVarP(&Scheme, "scheme", "s", "tcp", "Connection scheme")
	exposeCmd.Flags().StringVarP(&ServerHostOverride, "server-host-override", "o", "", "Server name use to verify the hostname returned by the TLS handshake")
	exposeCmd.Flags().StringVarP(&Namespace, "namespace", "n", "default", "Namespace")
	exposeCmd.Flags().StringVarP(&ServerImage, "server-image", "i", fmt.Sprintf("%s:v%s", k8s.Image, version), "Ktunnel server image to use")
	exposeCmd.Flags().BoolVarP(&Reuse, "reuse", "r", false, "deployment & service will be reused if exists or they will be created (tunnel)")
	exposeCmd.Flags().StringSliceVarP(&NodeSelectorTags,"node-selector-tags", "q", []string{}, "tag and value seperated by the '=' character (i.e kubernetes.io/os=linux)")
	rootCmd.AddCommand(exposeCmd)
}
