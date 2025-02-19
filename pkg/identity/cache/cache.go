// Copyright 2016-2018 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cache

import (
	"context"
	"reflect"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/pkg/allocator"
	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/idpool"
	"github.com/cilium/cilium/pkg/kvstore"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
)

var (
	log = logging.DefaultLogger.WithField(logfields.LogSubsys, "identity-cache")
)

// IdentityCache is a cache of identity to labels mapping
type IdentityCache map[identity.NumericIdentity]labels.LabelArray

// IdentitiesModel is a wrapper so that we can implement the sort.Interface
// to sort the slice by ID
type IdentitiesModel []*models.Identity

// Less returns true if the element in index `i` is lower than the element
// in index `j`
func (s IdentitiesModel) Less(i, j int) bool {
	return s[i].ID < s[j].ID
}

// GetIdentityCache returns a cache of all known identities
func (m *CachingIdentityAllocator) GetIdentityCache() IdentityCache {
	log.Debug("getting identity cache for identity allocator manager")
	cache := IdentityCache{}

	if m.IdentityAllocator != nil {

		m.IdentityAllocator.ForeachCache(func(id idpool.ID, val allocator.AllocatorKey) {
			if val != nil {
				if gi, ok := val.(GlobalIdentity); ok {
					cache[identity.NumericIdentity(id)] = gi.LabelArray
				} else {
					log.Warningf("Ignoring unknown identity type '%s': %+v",
						reflect.TypeOf(val), val)
				}
			}
		})
	}

	for key, identity := range identity.ReservedIdentityCache {
		cache[key] = identity.Labels.LabelArray()
	}

	if m.localIdentities != nil {
		for _, identity := range m.localIdentities.GetIdentities() {
			cache[identity.ID] = identity.Labels.LabelArray()
		}
	}

	return cache
}

// GetIdentities returns all known identities
func (m *CachingIdentityAllocator) GetIdentities() IdentitiesModel {
	identities := IdentitiesModel{}

	m.IdentityAllocator.ForeachCache(func(id idpool.ID, val allocator.AllocatorKey) {
		if gi, ok := val.(GlobalIdentity); ok {
			identity := identity.NewIdentityFromLabelArray(identity.NumericIdentity(id), gi.LabelArray)
			identities = append(identities, identity.GetModel())
		}

	})
	// append user reserved identities
	for _, v := range identity.ReservedIdentityCache {
		identities = append(identities, v.GetModel())
	}

	for _, v := range m.localIdentities.GetIdentities() {
		identities = append(identities, v.GetModel())
	}

	return identities
}

type identityWatcher struct {
	stopChan chan bool
	owner    IdentityAllocatorOwner
}

// collectEvent records the 'event' as an added or deleted identity,
// and makes sure that any identity is present in only one of the sets
// (added or deleted).
func collectEvent(event allocator.AllocatorEvent, added, deleted IdentityCache) bool {
	id := identity.NumericIdentity(event.ID)
	// Only create events have the key
	if event.Typ == kvstore.EventTypeCreate {
		if gi, ok := event.Key.(GlobalIdentity); ok {
			// Un-delete the added ID if previously
			// 'deleted' so that collected events can be
			// processed in any order.
			if _, exists := deleted[id]; exists {
				delete(deleted, id)
			}
			added[id] = gi.LabelArray
			return true
		}
		log.Warningf("collectEvent: Ignoring unknown identity type '%s': %+v",
			reflect.TypeOf(event.Key), event.Key)
		return false
	}
	// Reverse an add when subsequently deleted
	if _, exists := added[id]; exists {
		delete(added, id)
	}
	// record the id deleted even if an add was reversed, as the
	// id may also have previously existed, in which case the
	// result is not no-op!
	deleted[id] = labels.LabelArray{}

	return true
}

// watch starts the identity watcher
func (w *identityWatcher) watch(events allocator.AllocatorEventChan) {

	go func() {
		for {
			added := IdentityCache{}
			deleted := IdentityCache{}

		First:
			for {
				// Wait for one identity add or delete or stop
				select {
				case event, ok := <-events:
					if !ok {
						// 'events' was closed
						return
					}
					// Collect first added and deleted labels
					switch event.Typ {
					case kvstore.EventTypeCreate, kvstore.EventTypeDelete:
						if collectEvent(event, added, deleted) {
							// First event collected
							break First
						}
					default:
						// Ignore modify events
					}
				case <-w.stopChan:
					return
				}
			}

		More:
			for {
				// see if there is more, but do not wait nor stop
				select {
				case event, ok := <-events:
					if !ok {
						// 'events' was closed
						break More
					}
					// Collect more added and deleted labels
					switch event.Typ {
					case kvstore.EventTypeCreate, kvstore.EventTypeDelete:
						collectEvent(event, added, deleted)
					default:
						// Ignore modify events
					}
				default:
					// No more events available without blocking
					break More
				}
			}
			// Issue collected updates
			w.owner.UpdateIdentities(added, deleted) // disjoint sets
		}
	}()
}

// stop stops the identity watcher
func (w *identityWatcher) stop() {
	close(w.stopChan)
}

// LookupIdentity looks up the identity by its labels but does not create it.
// This function will first search through the local cache and fall back to
// querying the kvstore.
func (m *CachingIdentityAllocator) LookupIdentity(lbls labels.Labels) *identity.Identity {
	if reservedIdentity := identity.LookupReservedIdentityByLabels(lbls); reservedIdentity != nil {
		return reservedIdentity
	}

	if identity := m.localIdentities.lookup(lbls); identity != nil {
		return identity
	}

	if m.IdentityAllocator == nil {
		return nil
	}

	lblArray := lbls.LabelArray()
	id, err := m.IdentityAllocator.Get(context.TODO(), GlobalIdentity{lblArray})
	if err != nil {
		return nil
	}

	if id == idpool.NoID {
		return nil
	}

	return identity.NewIdentityFromLabelArray(identity.NumericIdentity(id), lblArray)
}

var unknownIdentity = identity.NewIdentity(identity.IdentityUnknown, labels.Labels{labels.IDNameUnknown: labels.NewLabel(labels.IDNameUnknown, "", labels.LabelSourceReserved)})

// LookupIdentityByID returns the identity by ID. This function will first
// search through the local cache and fall back to querying the kvstore.
func (m *CachingIdentityAllocator) LookupIdentityByID(id identity.NumericIdentity) *identity.Identity {
	if id == identity.IdentityUnknown {
		return unknownIdentity
	}

	if identity := identity.LookupReservedIdentity(id); identity != nil {
		return identity
	}

	if m.IdentityAllocator == nil {
		return nil
	}

	if identity := m.localIdentities.lookupByID(id); identity != nil {
		return identity
	}

	allocatorKey, err := m.IdentityAllocator.GetByID(idpool.ID(id))
	if err != nil {
		return nil
	}

	if gi, ok := allocatorKey.(GlobalIdentity); ok {
		return identity.NewIdentityFromLabelArray(id, gi.LabelArray)
	}

	return nil
}
