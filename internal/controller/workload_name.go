package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	plexusv1alpha1 "github.com/AnthonyPoschen/plexus-controller/api/v1alpha1"
)

var errWorkloadNameCollision = errors.New("workload deployment name is already used by another GameServer")

const (
	workloadNameSlugMax = 16
	workloadNameMax     = 63
)

// workloadDeploymentName is the Deployment metadata.name we set:
// <customer>-<server>-<game>. Kubernetes may append a ReplicaSet suffix.
// Do not add a server-id hash of our own.
func workloadDeploymentName(gameServer *plexusv1alpha1.GameServer) (string, error) {
	if gameServer == nil || gameServer.Spec.SelectedSetup == nil {
		return "", fmt.Errorf("workload name requires a selected setup")
	}
	game := dns1123Slug(gameServer.Spec.SelectedSetup.GameID, workloadNameSlugMax)
	if game == "" {
		return "", fmt.Errorf("selected setup gameID %q is not a DNS-1123 name", gameServer.Spec.SelectedSetup.GameID)
	}
	customer := customerNameSlug(gameServer.Spec.CustomerSlug, gameServer.Spec.OwnerUserID)
	if customer == "" {
		return "", fmt.Errorf("customer slug is empty")
	}
	server := dns1123Slug(gameServer.Spec.DisplayName, workloadNameSlugMax)
	if server == "" {
		server = "server"
	}
	name := customer + "-" + server + "-" + game
	if len(name) > workloadNameMax {
		return "", fmt.Errorf("workload name %q exceeds %d characters", name, workloadNameMax)
	}
	return name, nil
}

func customerNameSlug(raw, ownerUserID string) string {
	if i := strings.Index(raw, "@"); i >= 0 {
		raw = raw[:i]
	}
	if slug := dns1123Slug(raw, workloadNameSlugMax); slug != "" {
		return slug
	}
	owner := dns1123Slug(ownerUserID, 32)
	if len(owner) > 8 {
		owner = owner[:8]
	}
	return dns1123Slug("u"+owner, workloadNameSlugMax)
}

func dns1123Slug(value string, max int) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case r == '-' || r == '_' || r == '.' || unicode.IsSpace(r):
			if b.Len() == 0 || lastHyphen {
				continue
			}
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if max > 0 && len(out) > max {
		out = strings.TrimRight(out[:max], "-")
	}
	return out
}

func isWorkloadNameCollision(err error) bool {
	return errors.Is(err, errWorkloadNameCollision)
}

func (r *GameServerReconciler) ensureWorkloadNameAvailable(ctx context.Context, gameServer *plexusv1alpha1.GameServer, name string) error {
	existing := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: gameServer.Namespace, Name: name}, existing); err != nil {
		return client.IgnoreNotFound(err)
	}
	if existing.Labels[plexusv1alpha1.LabelGameServerUID] == string(gameServer.UID) {
		return nil
	}
	if controller := metav1.GetControllerOf(existing); controller != nil && controller.UID == gameServer.UID {
		return nil
	}
	return fmt.Errorf("%w: %s/%s", errWorkloadNameCollision, gameServer.Namespace, name)
}

func (r *GameServerReconciler) deleteStaleWorkloadDeployments(ctx context.Context, gameServer *plexusv1alpha1.GameServer, currentName string) error {
	if currentName == "" {
		return nil
	}
	deployments := &appsv1.DeploymentList{}
	if err := r.List(ctx, deployments, client.InNamespace(gameServer.Namespace), client.MatchingLabels{
		plexusv1alpha1.LabelGameServerUID: string(gameServer.UID),
	}); err != nil {
		return err
	}
	seen := map[string]struct{}{}
	for i := range deployments.Items {
		item := &deployments.Items[i]
		seen[item.Name] = struct{}{}
		if err := r.deleteStaleWorkloadDeployment(ctx, gameServer, item, currentName); err != nil {
			return err
		}
	}
	if currentName == gameServer.Name {
		return nil
	}
	if _, ok := seen[gameServer.Name]; ok {
		return nil
	}
	legacy := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: gameServer.Namespace, Name: gameServer.Name}, legacy); err != nil {
		return client.IgnoreNotFound(err)
	}
	return r.deleteStaleWorkloadDeployment(ctx, gameServer, legacy, currentName)
}

func (r *GameServerReconciler) deleteStaleWorkloadDeployment(ctx context.Context, gameServer *plexusv1alpha1.GameServer, deployment *appsv1.Deployment, currentName string) error {
	if deployment.Name == currentName {
		return nil
	}
	if err := ensureControlledBy(gameServer, deployment); err != nil {
		return nil
	}
	if !deployment.DeletionTimestamp.IsZero() {
		return nil
	}
	return r.Delete(ctx, deployment)
}

func (r *GameServerReconciler) ownedWorkloadDeployments(ctx context.Context, gameServer *plexusv1alpha1.GameServer) ([]appsv1.Deployment, error) {
	listed := &appsv1.DeploymentList{}
	if err := r.List(ctx, listed, client.InNamespace(gameServer.Namespace), client.MatchingLabels{
		plexusv1alpha1.LabelGameServerUID: string(gameServer.UID),
	}); err != nil {
		return nil, err
	}
	owned := make([]appsv1.Deployment, 0, len(listed.Items)+2)
	seen := map[string]struct{}{}
	for i := range listed.Items {
		item := listed.Items[i]
		if err := ensureControlledBy(gameServer, &item); err != nil {
			continue
		}
		seen[item.Name] = struct{}{}
		owned = append(owned, item)
	}
	for _, name := range workloadDeploymentLookupNames(gameServer) {
		if _, ok := seen[name]; ok {
			continue
		}
		deployment := appsv1.Deployment{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: gameServer.Namespace, Name: name}, &deployment); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return nil, err
			}
			continue
		}
		if err := ensureControlledBy(gameServer, &deployment); err != nil {
			continue
		}
		owned = append(owned, deployment)
	}
	return owned, nil
}

func workloadDeploymentLookupNames(gameServer *plexusv1alpha1.GameServer) []string {
	names := []string{gameServer.Name}
	if name, err := workloadDeploymentName(gameServer); err == nil && name != gameServer.Name {
		names = append(names, name)
	}
	return names
}
