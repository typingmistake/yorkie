/*
 * Copyright 2020 The Yorkie Authors. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package document provides JSON-like document(CRDT) implementation.
package document

import (
	gojson "encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/yorkie-team/yorkie/api/types"
	"github.com/yorkie-team/yorkie/pkg/document/change"
	"github.com/yorkie-team/yorkie/pkg/document/crdt"
	"github.com/yorkie-team/yorkie/pkg/document/innerpresence"
	"github.com/yorkie-team/yorkie/pkg/document/json"
	"github.com/yorkie-team/yorkie/pkg/document/key"
	"github.com/yorkie-team/yorkie/pkg/document/presence"
	"github.com/yorkie-team/yorkie/pkg/document/time"
	"github.com/yorkie-team/yorkie/pkg/resource"
	"github.com/yorkie-team/yorkie/pkg/schema"
)

var (
	// ErrUnsupportedPayloadType is returned when the payload is unserializable to JSON.
	ErrUnsupportedPayloadType = errors.New("unsupported payload type")

	// ErrDocumentSizeExceedsLimit is returned when the document size exceeds the limit.
	ErrDocumentSizeExceedsLimit = errors.New("document size exceeds the limit")

	// ErrSchemaValidationFailed is returned when the document schema validation failed.
	ErrSchemaValidationFailed = errors.New("schema validation failed")
)

// DocEvent represents the event that occurred in the document.
type DocEvent struct {
	Type      DocEventType
	Presences map[string]innerpresence.Presence
}

// DocEventType represents the type of the event that occurred in the document.
type DocEventType string

const (
	// WatchedEvent means that the client has established a connection with the server,
	// enabling real-time synchronization.
	WatchedEvent DocEventType = "watched"

	// UnwatchedEvent means that the client has disconnected from the server,
	// disabling real-time synchronization.
	UnwatchedEvent DocEventType = "unwatched"

	// PresenceChangedEvent means that the presences of the clients who are editing
	// the document have changed.
	PresenceChangedEvent DocEventType = "presence-changed"
)

// BroadcastRequest represents a broadcast request that will be delivered to the client.
type BroadcastRequest struct {
	Topic   string
	Payload []byte
}

// Option configures Options.
type Option func(*Options)

// Options configures how we set up the document.
type Options struct {
	// DisableGC disables garbage collection.
	// NOTE(hackerwins): This is temporary option. We need to remove this option
	// after introducing the garbage collection based on the version vector.
	DisableGC bool
}

// WithDisableGC configures the document to disable garbage collection.
func WithDisableGC() Option {
	return func(o *Options) {
		o.DisableGC = true
	}
}

// Document represents a document accessible to the user.
//
// How document works:
// The operations are generated by the proxy while executing user's command on
// the clone. Then the operations will apply the changes into the base json
// root. This is to protect the base json from errors that may occur while user
// edit the document.
type Document struct {
	// doc is the original data of the actual document.
	doc *InternalDocument

	// options is the options to configure the document.
	options Options

	// cloneRoot is a copy of `doc.root` to be exposed to the user and is used to
	// protect `doc.root`.
	cloneRoot *crdt.Root

	// clonePresences is a copy of `doc.presences` to be exposed to the user and
	// is used to protect `doc.presences`.
	clonePresences *innerpresence.Map

	// MaxSizeLimit is the maximum size of a document in bytes.
	MaxSizeLimit int

	// SchemaRules is the rules of the schema of the document.
	SchemaRules []types.Rule

	// events is the channel to send events that occurred in the document.
	events chan DocEvent

	// broadcastRequests is the send-only channel to send broadcast requests.
	broadcastRequests chan BroadcastRequest

	// broadcastResponses is the receive-only channel to receive broadcast responses.
	broadcastResponses chan error

	// broadcastEventHandlers is a map of registered event handlers for events.
	broadcastEventHandlers map[string]func(
		topic, publisher string,
		payload []byte) error
}

// New creates a new instance of Document.
func New(key key.Key, opts ...Option) *Document {
	var options Options
	for _, opt := range opts {
		opt(&options)
	}

	return &Document{
		doc:                NewInternalDocument(key),
		options:            options,
		events:             make(chan DocEvent, 1),
		broadcastRequests:  make(chan BroadcastRequest, 1),
		broadcastResponses: make(chan error, 1),
		broadcastEventHandlers: make(map[string]func(
			topic, publisher string,
			payload []byte) error),
	}
}

// Update executes the given updater to update this document.
func (d *Document) Update(
	updater func(root *json.Object, p *presence.Presence) error,
	msgAndArgs ...interface{},
) error {
	if d.doc.status == StatusRemoved {
		return ErrDocumentRemoved
	}

	if err := d.ensureClone(); err != nil {
		return err
	}

	ctx := change.NewContext(
		d.doc.changeID,
		messageFromMsgAndArgs(msgAndArgs...),
		d.cloneRoot,
	)

	if err := updater(
		json.NewObject(ctx, d.cloneRoot.Object()),
		presence.New(ctx, d.clonePresences.LoadOrStore(d.ActorID().String(), innerpresence.New())),
	); err != nil {
		// NOTE(hackerwins): If the updater fails, we need to remove the cloneRoot and
		// clonePresences to prevent the user from accessing the invalid state.
		d.cloneRoot = nil
		d.clonePresences = nil
		return err
	}

	if !ctx.IsPresenceOnlyChange() && len(d.SchemaRules) > 0 {
		result := schema.ValidateYorkieRuleset(d.cloneRoot.Object(), d.SchemaRules)
		if !result.Valid {
			var errorMessages []string
			for _, err := range result.Errors {
				errorMessages = append(errorMessages, err.Message)
			}
			d.cloneRoot = nil
			d.clonePresences = nil
			return fmt.Errorf("%w: %s", ErrSchemaValidationFailed, strings.Join(errorMessages, ", "))
		}
	}

	cloneSize := d.cloneRoot.DocSize()
	if !ctx.IsPresenceOnlyChange() && d.MaxSizeLimit > 0 && d.MaxSizeLimit < cloneSize.Total() {
		// NOTE(hackerwins): If the updater fails, we need to remove the cloneRoot and
		// clonePresences to prevent the user from accessing the invalid state.
		d.cloneRoot = nil
		d.clonePresences = nil
		return ErrDocumentSizeExceedsLimit
	}

	if ctx.HasChange() {
		c := ctx.ToChange()
		if err := c.Execute(d.doc.root, d.doc.presences); err != nil {
			return err
		}

		d.doc.localChanges = append(d.doc.localChanges, c)
		d.doc.changeID = ctx.NextID()
	}

	return nil
}

// ApplyChangePack applies the given change pack into this document.
func (d *Document) ApplyChangePack(pack *change.Pack) error {
	// 01. Apply remote changes to both the cloneRoot and the document.
	hasSnapshot := len(pack.Snapshot) > 0

	if hasSnapshot {
		d.cloneRoot = nil
		d.clonePresences = nil
		if err := d.doc.applySnapshot(pack.Snapshot, pack.VersionVector); err != nil {
			return err
		}
	} else {
		if err := d.applyChanges(pack.Changes); err != nil {
			return err
		}
	}

	// 02. Remove local changes applied to server.
	for d.HasLocalChanges() {
		c := d.doc.localChanges[0]
		if c.ClientSeq() > pack.Checkpoint.ClientSeq {
			break
		}
		d.doc.localChanges = d.doc.localChanges[1:]
	}

	if len(pack.Snapshot) > 0 {
		if err := d.applyChanges(d.doc.localChanges); err != nil {
			return err
		}
	}

	// 03. Update the checkpoint.
	d.doc.checkpoint = d.doc.checkpoint.Forward(pack.Checkpoint)

	// 04. Do Garbage collection.
	if !d.options.DisableGC && !hasSnapshot {
		d.GarbageCollect(pack.VersionVector)
	}

	// 05. Update the status.
	if pack.IsRemoved {
		d.SetStatus(StatusRemoved)
	}

	return nil
}

func (d *Document) applyChanges(changes []*change.Change) error {
	if err := d.ensureClone(); err != nil {
		return err
	}

	for _, c := range changes {
		if err := c.Execute(d.cloneRoot, d.clonePresences); err != nil {
			return err
		}
	}

	events, err := d.doc.ApplyChanges(changes...)
	if err != nil {
		return err
	}

	for _, e := range events {
		d.events <- e
	}
	return nil
}

// InternalDocument returns the internal document.
func (d *Document) InternalDocument() *InternalDocument {
	return d.doc
}

// Key returns the key of this document.
func (d *Document) Key() key.Key {
	return d.doc.key
}

// Checkpoint returns the checkpoint of this document.
func (d *Document) Checkpoint() change.Checkpoint {
	return d.doc.checkpoint
}

// HasLocalChanges returns whether this document has local changes or not.
func (d *Document) HasLocalChanges() bool {
	return d.doc.HasLocalChanges()
}

// Marshal returns the JSON encoding of this document.
func (d *Document) Marshal() string {
	return d.doc.Marshal()
}

// CreateChangePack creates pack of the local changes to send to the server.
func (d *Document) CreateChangePack() *change.Pack {
	return d.doc.CreateChangePack()
}

// SetActor sets actor into this document. This is also applied in the local
// changes the document has.
func (d *Document) SetActor(actor time.ActorID) {
	d.doc.SetActor(actor)
}

// ActorID returns ID of the actor currently editing the document.
func (d *Document) ActorID() time.ActorID {
	return d.doc.ActorID()
}

// SetStatus updates the status of this document.
func (d *Document) SetStatus(status StatusType) {
	d.doc.SetStatus(status)
}

// VersionVector returns the version vector of this document.
func (d *Document) VersionVector() time.VersionVector {
	return d.doc.VersionVector()
}

// Status returns the status of this document.
func (d *Document) Status() StatusType {
	return d.doc.status
}

// IsAttached returns whether this document is attached or not.
func (d *Document) IsAttached() bool {
	return d.doc.IsAttached()
}

// RootObject returns the internal root object of this document.
func (d *Document) RootObject() *crdt.Object {
	return d.doc.RootObject()
}

// Root returns the root object of this document.
func (d *Document) Root() *json.Object {
	if err := d.ensureClone(); err != nil {
		panic(err)
	}

	ctx := change.NewContext(d.doc.changeID.Next(), "", d.cloneRoot)
	return json.NewObject(ctx, d.cloneRoot.Object())
}

// DocSize returns the size of this document.
func (d *Document) DocSize() resource.DocSize {
	return d.doc.root.DocSize()
}

// GarbageCollect purge elements that were removed before the given time.
func (d *Document) GarbageCollect(vector time.VersionVector) int {
	if d.cloneRoot != nil {
		if _, err := d.cloneRoot.GarbageCollect(vector); err != nil {
			panic(err)
		}
	}

	n, err := d.doc.GarbageCollect(vector)
	if err != nil {
		panic(err)
	}

	return n
}

// GarbageLen returns the count of removed elements.
func (d *Document) GarbageLen() int {
	return d.doc.GarbageLen()
}

func (d *Document) ensureClone() error {
	if d.cloneRoot == nil {
		copiedDoc, err := d.doc.root.DeepCopy()
		if err != nil {
			return err
		}
		d.cloneRoot = copiedDoc
	}

	if d.clonePresences == nil {
		d.clonePresences = d.doc.presences.DeepCopy()
	}

	return nil
}

// MyPresence returns the presence of the actor.
func (d *Document) MyPresence() innerpresence.Presence {
	return d.doc.MyPresence()
}

// Presence returns the presence of the given client.
// If the client is not online, it returns nil.
func (d *Document) Presence(clientID string) innerpresence.Presence {
	return d.doc.Presence(clientID)
}

// PresenceForTest returns the presence of the given client
// regardless of whether the client is online or not.
func (d *Document) PresenceForTest(clientID string) innerpresence.Presence {
	return d.doc.PresenceForTest(clientID)
}

// Presences returns the presence map of online clients.
func (d *Document) Presences() map[string]innerpresence.Presence {
	// TODO(hackerwins): We need to use client key instead of actor ID for exposing presence.
	return d.doc.Presences()
}

// AllPresences returns the presence map of all clients
// regardless of whether the client is online or not.
func (d *Document) AllPresences() map[string]innerpresence.Presence {
	return d.doc.AllPresences()
}

// SetOnlineClients sets the online clients.
func (d *Document) SetOnlineClients(clientIDs ...string) {
	d.doc.SetOnlineClients(clientIDs...)
}

// AddOnlineClient adds the given client to the online clients.
func (d *Document) AddOnlineClient(clientID string) {
	d.doc.AddOnlineClient(clientID)
}

// RemoveOnlineClient removes the given client from the online clients.
func (d *Document) RemoveOnlineClient(clientID string) {
	d.doc.RemoveOnlineClient(clientID)
}

// Events returns the events of this document.
func (d *Document) Events() <-chan DocEvent {
	return d.events
}

// BroadcastRequests returns the broadcast requests of this document.
func (d *Document) BroadcastRequests() <-chan BroadcastRequest {
	return d.broadcastRequests
}

// BroadcastResponses returns the broadcast responses of this document.
func (d *Document) BroadcastResponses() chan error {
	return d.broadcastResponses
}

// Broadcast encodes the given payload and sends a Broadcast request.
func (d *Document) Broadcast(topic string, payload any) error {
	marshaled, err := gojson.Marshal(payload)
	if err != nil {
		return ErrUnsupportedPayloadType
	}

	d.broadcastRequests <- BroadcastRequest{
		Topic:   topic,
		Payload: marshaled,
	}
	return <-d.broadcastResponses
}

// SubscribeBroadcastEvent subscribes to the given topic and registers
// an event handler.
func (d *Document) SubscribeBroadcastEvent(
	topic string,
	handler func(topic, publisher string, payload []byte) error,
) {
	d.broadcastEventHandlers[topic] = handler
}

// UnsubscribeBroadcastEvent unsubscribes to the given topic and deregisters
// the event handler.
func (d *Document) UnsubscribeBroadcastEvent(
	topic string,
) {
	delete(d.broadcastEventHandlers, topic)
}

// BroadcastEventHandlers returns the registered handlers for broadcast events.
func (d *Document) BroadcastEventHandlers() map[string]func(
	topic string,
	publisher string,
	payload []byte,
) error {
	return d.broadcastEventHandlers
}

func (d *Document) setInternalDoc(internalDoc *InternalDocument) {
	d.doc = internalDoc
}

func messageFromMsgAndArgs(msgAndArgs ...interface{}) string {
	if len(msgAndArgs) == 0 {
		return ""
	}
	if len(msgAndArgs) == 1 {
		msg := msgAndArgs[0]
		if msgAsStr, ok := msg.(string); ok {
			return msgAsStr
		}
		return fmt.Sprintf("%+v", msg)
	}
	if len(msgAndArgs) > 1 {
		return fmt.Sprintf(msgAndArgs[0].(string), msgAndArgs[1:]...)
	}
	return ""
}
