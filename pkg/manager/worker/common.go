package worker

import (
	"context"
	"fmt"
	log "log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/services"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

type Common struct {
	arpMgr       *arp.Manager
	cpCluster    *cluster.Cluster
	intfMgr      *networkinterface.Manager
	config       *kubevip.Config
	closing      *atomic.Bool
	signalChan   chan os.Signal
	svcProcessor *services.Processor
	mutex        *sync.Mutex
	clientSet    *kubernetes.Clientset
	leaseMgr     *lease.Manager
}

func (c *Common) InitControlPlane() error {
	var err error
	c.cpCluster, err = cluster.InitCluster(c.config, false, c.intfMgr, c.arpMgr)
	if err != nil {
		return fmt.Errorf("cluster initialization error: %w", err)
	}
	return nil
}

func (c *Common) ServicesPerServiceLeader(ctx context.Context) error {
	log.Info("beginning watching services, leaderelection will happen for every service")
	err := c.svcProcessor.StartServicesWatchForLeaderElection(ctx)
	if err != nil {
		return err
	}
	return nil
}

func (c *Common) ServicesGlobalLeader(ctx context.Context, id string) {
	ns, err := returnNameSpace()
	if err != nil {
		log.Warn("unable to auto-detect namespace, dropping to config", "namespace", c.config.Namespace)
		ns = c.config.Namespace
	}

	objectName := fmt.Sprintf("%s/%s-svcs", ns, c.config.ServicesLeaseName)
	objLease, newLease, sharedLease := c.leaseMgr.Add(c.config.ServicesLeaseName, objectName)
	// this service was already processed so we do not need to do anything
	if !newLease {
		log.Debug("this election was already done, waiting for it to finish", "lease", c.config.ServicesLeaseName)
		// Wait for either the service context or lease context to be done
		select {
		case <-ctx.Done():
			// Service was deleted
			c.leaseMgr.Delete(c.config.ServicesLeaseName, objectName)
		case <-objLease.Ctx.Done():
			// Leader election ended (leadership lost or context cancelled)
		}
		return
	}

	// Start a goroutine that will delete the lease when the service context is cancelled.
	// This is important for proper cleanup when a service is deleted - it ensures that
	// the lease context (svcLease.Ctx) gets cancelled, which causes RunOrDie to return.
	// Without this, RunOrDie would continue running until leadership is naturally lost.
	go func() {
		<-ctx.Done()
		c.leaseMgr.Delete(c.config.ServicesLeaseName, objectName)
	}()

	// this object is sharing lease with another object
	if sharedLease {
		log.Debug("this election was already done, shared lease", "lease", c.config.ServicesLeaseName)
		// wait for leader election to start or context to be done
		select {
		case <-objLease.Started:
		case <-objLease.Ctx.Done():
			// Lease was cancelled (e.g., leader election ended), return immediately
			// This allows the restart loop to create a fresh lease
			log.Debug("lease context cancelled before leader election started", "lease", c.config.ServicesLeaseName)
			return
		}

		err = c.svcProcessor.ServicesWatcher(ctx, c.svcProcessor.SyncServices)
		if err != nil {
			log.Error("service watcher", "err", err)
			if !c.closing.Load() {
				c.signalChan <- syscall.SIGINT
			}
			objLease.Cancel()
		}

		log.Debug("waiting for context DONE", "lease", c.config.ServicesLeaseName)
		// Block until context is cancelled
		<-ctx.Done()

		log.Debug("waiting for lease DONE", "lease", c.config.ServicesLeaseName)
		// wait for leaderelection to be finished
		<-objLease.Ctx.Done()

		// we can do cleanup here
		c.mutex.Lock()
		defer c.mutex.Unlock()
		log.Info("leader lost")
		c.svcProcessor.Stop()

		log.Error("lost services leadership, restarting kube-vip")
		if !c.closing.Load() {
			c.signalChan <- syscall.SIGINT
		}

		return
	}

	// For new leases (not shared), ensure cleanup when the leader election ends
	// This is critical for the restartable service watcher to be able to restart
	// the leader election after leadership loss
	defer func() {
		// Delete the lease from the manager so subsequent calls can create a fresh lease
		// This handles the case where leader election ends due to:
		// 1. Leadership loss (e.g., network timeout)
		// 2. Context cancellation
		// 3. Any other reason RunOrDie returns
		c.leaseMgr.Delete(c.config.ServicesLeaseName, objectName)
	}()

	log.Info("beginning services leadership", "namespace", ns, "lock name", c.config.ServicesLeaseName, "id", id)
	// we use the Lease lock type since edits to Leases are less common
	// and fewer objects in the cluster watch "all Leases".
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      c.config.ServicesLeaseName,
			Namespace: ns,
		},
		Client: c.clientSet.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}

	// start the leader election code loop
	leaderelection.RunOrDie(objLease.Ctx, leaderelection.LeaderElectionConfig{
		Lock: lock,
		// IMPORTANT: you MUST ensure that any code you have that
		// is protected by the lease must terminate **before**
		// you call cancel. Otherwise, you could have a background
		// loop still running and another process could
		// get elected before your background loop finished, violating
		// the stated goal of the lease.
		ReleaseOnCancel: true,
		LeaseDuration:   time.Duration(c.config.LeaseDuration) * time.Second,
		RenewDeadline:   time.Duration(c.config.RenewDeadline) * time.Second,
		RetryPeriod:     time.Duration(c.config.RetryPeriod) * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				close(objLease.Started)
				err = c.svcProcessor.ServicesWatcher(ctx, c.svcProcessor.SyncServices)
				if err != nil {
					log.Error("service watcher", "err", err)
					if !c.closing.Load() {
						c.signalChan <- syscall.SIGINT
					}
					objLease.Cancel()
				}
			},
			OnStoppedLeading: func() {
				// we can do cleanup here
				c.mutex.Lock()
				defer c.mutex.Unlock()
				log.Info("leader lost", "new leader", id)
				c.svcProcessor.Stop()

				log.Error("lost services leadership, restarting kube-vip")
				if !c.closing.Load() {
					c.signalChan <- syscall.SIGINT
				}
			},
			OnNewLeader: func(identity string) {
				// we're notified when new leader elected
				if c.config.EnableNodeLabeling {
					applyNodeLabel(ctx, c.clientSet, c.config.Address, id, identity)
				}
				if identity == id {
					// I just got the lock
					return
				}
				log.Info("new leader elected", "new leader", identity)
			},
		},
	})
}

func (c *Common) ServicesNoLeader(ctx context.Context) error {
	log.Info("beginning watching services without leader election")
	err := c.svcProcessor.ServicesWatcher(ctx, c.svcProcessor.SyncServices)
	if err != nil {
		return fmt.Errorf("error while watching services: %w", err)
	}
	return nil
}

func returnNameSpace() (string, error) {
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns, nil
		}
		return "", err
	}
	return "", fmt.Errorf("unable to find Namespace")
}
