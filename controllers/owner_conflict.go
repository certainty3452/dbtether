package controllers

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

// Oldest CreationTimestamp wins, ties by namespace/name, so every controller elects the same owner.
func ElectOwner(users []databasesv1alpha1.DatabaseUser, dbNamespace, dbName string) *databasesv1alpha1.DatabaseUser {
	var winner *databasesv1alpha1.DatabaseUser
	for i := range users {
		candidate := &users[i]
		// A stuck finalizer must not block the next owner.
		if !candidate.DeletionTimestamp.IsZero() {
			continue
		}
		// An invalid spec never reaches ApplyPrivileges, so it must not hold the claim either.
		if ValidateUserSpec(candidate) != nil {
			continue
		}
		grants, ok := ResolveUserGrantsForDatabase(candidate, dbNamespace, dbName)
		if !ok || grants.Privileges != "owner" {
			continue
		}
		if winner == nil || ownerCandidateWins(candidate, winner) {
			winner = candidate
		}
	}
	return winner
}

func SameUser(a, b *databasesv1alpha1.DatabaseUser) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Namespace == b.Namespace && a.Name == b.Name
}

// admin is the highest preset that transfers no object ownership.
func LoserGrants(grants UserGrants) UserGrants {
	grants.Privileges = "admin"
	return grants
}

type ownerLoss struct {
	database types.NamespacedName
	message  string
}

type ownerConflicts struct {
	losses    []ownerLoss
	contested bool
}

func (c ownerConflicts) empty() bool {
	return len(c.losses) == 0 && !c.contested
}

func (c ownerConflicts) lossFor(namespace, name string) string {
	for _, loss := range c.losses {
		if loss.database.Namespace == namespace && loss.database.Name == name {
			return loss.message
		}
	}
	return ""
}

func (c ownerConflicts) messages() []string {
	messages := make([]string, 0, len(c.losses))
	for _, loss := range c.losses {
		messages = append(messages, loss.message)
	}
	return messages
}

func ownedDatabaseRefs(user *databasesv1alpha1.DatabaseUser) []types.NamespacedName {
	var refs []types.NamespacedName
	for _, access := range user.Spec.GetDatabases() {
		if ResolveUserGrants(user, access).Privileges != "owner" {
			continue
		}
		refs = append(refs, types.NamespacedName{
			Namespace: databaseAccessNamespace(user, access),
			Name:      access.Name,
		})
	}
	return refs
}

func (r *DatabaseUserReconciler) evaluateOwnerConflicts(ctx context.Context,
	user *databasesv1alpha1.DatabaseUser) (ownerConflicts, error) {

	var conflicts ownerConflicts
	for _, db := range ownedDatabaseRefs(user) {
		others, err := r.listOwnerPeers(ctx, user, db)
		if err != nil {
			return ownerConflicts{}, err
		}

		other := ElectOwner(others, db.Namespace, db.Name)
		if other == nil {
			continue
		}
		if ownerCandidateWins(other, user) {
			conflicts.losses = append(conflicts.losses, ownerLoss{database: db, message: ownerConflictMessage(other, db)})
			continue
		}
		conflicts.contested = true
	}
	return conflicts, nil
}

// Wakes the other owners so a loser recovers on the holder's downgrade or deletion.
func (r *DatabaseUserReconciler) enqueueOwnerPeers(ctx context.Context, obj client.Object) []ctrl.Request {
	user, ok := obj.(*databasesv1alpha1.DatabaseUser)
	if !ok {
		return nil
	}

	var requests []ctrl.Request
	seen := make(map[types.NamespacedName]struct{})
	for _, db := range ownedDatabaseRefs(user) {
		others, err := r.listOwnerPeers(ctx, user, db)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to enqueue owner peers", "database", db.String())
			continue
		}

		for i := range others {
			grants, referenced := ResolveUserGrantsForDatabase(&others[i], db.Namespace, db.Name)
			if !referenced || grants.Privileges != "owner" {
				continue
			}
			peer := types.NamespacedName{Namespace: others[i].Namespace, Name: others[i].Name}
			if _, duplicate := seen[peer]; duplicate {
				continue
			}
			seen[peer] = struct{}{}
			requests = append(requests, ctrl.Request{NamespacedName: peer})
		}
	}
	return requests
}

// Namespace-wide List: a DatabaseUser may live elsewhere than the Database it points at.
func (r *DatabaseUserReconciler) listOwnerPeers(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	db types.NamespacedName) ([]databasesv1alpha1.DatabaseUser, error) {

	var users databasesv1alpha1.DatabaseUserList
	if err := r.List(ctx, &users, client.MatchingFields{
		DatabaseUserDatabaseRefIndex: DatabaseUserDatabaseRefKey(db.Namespace, db.Name),
	}); err != nil {
		return nil, fmt.Errorf("failed to list DatabaseUsers referencing %s: %w", db, err)
	}

	others := make([]databasesv1alpha1.DatabaseUser, 0, len(users.Items))
	for i := range users.Items {
		if users.Items[i].Namespace == user.Namespace && users.Items[i].Name == user.Name {
			continue
		}
		others = append(others, users.Items[i])
	}
	return others, nil
}

func ownerConflictMessage(holder *databasesv1alpha1.DatabaseUser, db types.NamespacedName) string {
	return fmt.Sprintf("owner conflict: DatabaseUser %s/%s already holds privileges=owner on Database %s/%s; only one owner per database",
		holder.Namespace, holder.Name, db.Namespace, db.Name)
}

func ownerCandidateWins(candidate, incumbent *databasesv1alpha1.DatabaseUser) bool {
	if !candidate.CreationTimestamp.Equal(&incumbent.CreationTimestamp) {
		return candidate.CreationTimestamp.Before(&incumbent.CreationTimestamp)
	}
	if candidate.Namespace != incumbent.Namespace {
		return candidate.Namespace < incumbent.Namespace
	}
	return candidate.Name < incumbent.Name
}
