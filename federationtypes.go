package gomatrixserverlib

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/matrix-org/util"
)

// A ServerName is the name a matrix homeserver is identified by.
// It is a DNS name or IP address optionally followed by a port.
//
// https://matrix.org/docs/spec/appendices.html#server-name
type ServerName string

// ParseAndValidateServerName splits a ServerName into a host and port part,
// and checks that it is a valid server name according to the spec.
//
// if there is no explicit port, returns '-1' as the port.
func ParseAndValidateServerName(serverName ServerName) (host string, port int, valid bool) {
	// Don't go any further if the server name is an empty string.
	if len(serverName) == 0 {
		return
	}

	host, port = splitServerName(serverName)

	// the host part must be one of:
	//  - a valid (ascii) dns name
	//  - an IPv4 address
	//  - an IPv6 address

	if host[0] == '[' {
		// must be a valid IPv6 address
		if host[len(host)-1] != ']' {
			return
		}
		ip := host[1 : len(host)-1]
		if net.ParseIP(ip) == nil {
			return
		}
		valid = true
		return
	}

	// try parsing as an IPv4 address
	ip := net.ParseIP(host)
	if ip != nil && ip.To4() != nil {
		valid = true
		return
	}

	// must be a valid DNS Name
	for _, r := range host {
		if !isDNSNameChar(r) {
			return
		}
	}

	valid = true
	return
}

func isDNSNameChar(r rune) bool {
	if r >= 'A' && r <= 'Z' {
		return true
	}
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= '0' && r <= '9' {
		return true
	}
	if r == '-' || r == '.' {
		return true
	}
	return false
}

// splitServerName splits a ServerName into host and port, without doing
// any validation.
//
// if there is no explicit port, returns '-1' as the port
func splitServerName(serverName ServerName) (string, int) {
	nameStr := string(serverName)

	lastColon := strings.LastIndex(nameStr, ":")
	if lastColon < 0 {
		// no colon: no port
		return nameStr, -1
	}

	portStr := nameStr[lastColon+1:]
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		// invalid port (possibly an ipv6 host)
		return nameStr, -1
	}

	return nameStr[:lastColon], int(port)
}

// A RespSend is the content of a response to PUT /_matrix/federation/v1/send/{txnID}/
type RespSend struct {
	// Map of event ID to the result of processing that event.
	PDUs map[string]PDUResult `json:"pdus"`
}

// A PDUResult is the result of processing a matrix room event.
type PDUResult struct {
	// If not empty then this is a human readable description of a problem
	// encountered processing an event.
	Error string `json:"error,omitempty"`
}

// A RespStateIDs is the content of a response to GET /_matrix/federation/v1/state_ids/{roomID}/{eventID}
type RespStateIDs struct {
	// A list of state event IDs for the state of the room before the requested event.
	StateEventIDs []string `json:"pdu_ids"`
	// A list of event IDs needed to authenticate the state events.
	AuthEventIDs []string `json:"auth_chain_ids"`
}

// A RespState is the content of a response to GET /_matrix/federation/v1/state/{roomID}/{eventID}
type RespState struct {
	// A list of events giving the state of the room before the request event.
	StateEvents []Event `json:"pdus"`
	// A list of events needed to authenticate the state events.
	AuthEvents []Event `json:"auth_chain"`
}

// RespPublicRooms is the content of a response to GET /_matrix/federation/v1/publicRooms
type RespPublicRooms struct {
	// A paginated chunk of public rooms.
	Chunk []PublicRoom `json:"chunk"`
	// A pagination token for the response. The absence of this token means there are no more results to fetch and the client should stop paginating.
	NextBatch string `json:"next_batch,omitempty"`
	// A pagination token that allows fetching previous results. The absence of this token means there are no results before this batch, i.e. this is the first batch.
	PrevBatch string `json:"prev_batch,omitempty"`
	// An estimate on the total number of public rooms, if the server has an estimate.
	TotalRoomCountEstimate int `json:"total_room_count_estimate,omitempty"`
}

// PublicRoom stores the info of a room returned by
// GET /_matrix/federation/v1/publicRooms
type PublicRoom struct {
	// Aliases of the room. May be empty.
	Aliases []string `json:"aliases,omitempty"`
	// The canonical alias of the room, if any.
	CanonicalAlias string `json:"canonical_alias,omitempty"`
	// The name of the room, if any.
	Name string `json:"name,omitempty"`
	// The number of members joined to the room.
	JoinedMembersCount int `json:"num_joined_members"`
	// The ID of the room.
	RoomID string `json:"room_id"`
	// The topic of the room, if any.
	Topic string `json:"topic,omitempty"`
	// Whether the room may be viewed by guest users without joining.
	WorldReadable bool `json:"world_readable"`
	// Whether guest users may join the room and participate in it. If they can, they will be subject to ordinary power level rules like any other user.
	GuestCanJoin bool `json:"guest_can_join"`
	// The URL for the room's avatar, if one is set.
	AvatarURL string `json:"avatar_url,omitempty"`
}

// A RespEventAuth is the content of a response to GET /_matrix/federation/v1/event_auth/{roomID}/{eventID}
type RespEventAuth struct {
	// A list of events needed to authenticate the state events.
	AuthEvents []Event `json:"auth_chain"`
}

// Events combines the auth events and the state events and returns
// them in an order where every event comes after its auth events.
// Each event will only appear once in the output list.
// Returns an error if there are missing auth events or if there is
// a cycle in the auth events.
func (r RespState) Events() ([]Event, error) {
	eventsByID := map[string]*Event{}
	// Collect a map of event reference to event
	for i := range r.StateEvents {
		eventsByID[r.StateEvents[i].EventID()] = &r.StateEvents[i]
	}
	for i := range r.AuthEvents {
		eventsByID[r.AuthEvents[i].EventID()] = &r.AuthEvents[i]
	}

	queued := map[*Event]bool{}
	outputted := map[*Event]bool{}
	var result []Event
	for _, event := range eventsByID {
		if outputted[event] {
			// If we've already written the event then we can skip it.
			continue
		}

		// The code below does a depth first scan through the auth events
		// looking for events that can be appended to the output.

		// We use an explicit stack rather than using recursion so
		// that we can check we aren't creating cycles.
		stack := []*Event{event}

	LoopProcessTopOfStack:
		for len(stack) > 0 {
			top := stack[len(stack)-1]
			// Check if we can output the top of the stack.
			// We can output it if we have outputted all of its auth_events.
			for _, ref := range top.AuthEvents() {
				authEvent := eventsByID[ref.EventID]
				if authEvent == nil {
					return nil, fmt.Errorf(
						"gomatrixserverlib: missing auth event with ID %q for event %q",
						ref.EventID, top.EventID(),
					)
				}
				if outputted[authEvent] {
					continue
				}
				if queued[authEvent] {
					return nil, fmt.Errorf(
						"gomatrixserverlib: auth event cycle for ID %q",
						ref.EventID,
					)
				}
				// If we haven't visited the auth event yet then we need to
				// process it before processing the event currently on top of
				// the stack.
				stack = append(stack, authEvent)
				queued[authEvent] = true
				continue LoopProcessTopOfStack
			}
			// If we've processed all the auth events for the event on top of
			// the stack then we can append it to the result and try processing
			// the item below it in the stack.
			result = append(result, *top)
			outputted[top] = true
			stack = stack[:len(stack)-1]
		}
	}

	return result, nil
}

// Check that a response to /state is valid.
func (r RespState) Check(ctx context.Context, keyRing JSONVerifier) error {
	logger := util.GetLogger(ctx)
	var allEvents []Event
	for _, event := range r.AuthEvents {
		if event.StateKey() == nil {
			return fmt.Errorf("gomatrixserverlib: event %q does not have a state key", event.EventID())
		}
		allEvents = append(allEvents, event)
	}

	stateTuples := map[StateKeyTuple]bool{}
	for _, event := range r.StateEvents {
		if event.StateKey() == nil {
			return fmt.Errorf("gomatrixserverlib: event %q does not have a state key", event.EventID())
		}
		stateTuple := StateKeyTuple{event.Type(), *event.StateKey()}
		if stateTuples[stateTuple] {
			return fmt.Errorf(
				"gomatrixserverlib: duplicate state key tuple (%q, %q)",
				event.Type(), *event.StateKey(),
			)
		}
		stateTuples[stateTuple] = true
		allEvents = append(allEvents, event)
	}

	// Check if the events pass signature checks.
	logger.Infof("Checking event signatures for %d events of room state", len(allEvents))
	if err := VerifyAllEventSignatures(ctx, allEvents, keyRing); err != nil {
		return err
	}

	eventsByID := map[string]*Event{}
	// Collect a map of event reference to event
	for i := range allEvents {
		eventsByID[allEvents[i].EventID()] = &allEvents[i]
	}

	// Check whether the events are allowed by the auth rules.
	for _, event := range allEvents {
		if err := checkAllowedByAuthEvents(event, eventsByID); err != nil {
			return err
		}
	}

	return nil
}

// A RespMakeJoin is the content of a response to GET /_matrix/federation/v2/make_join/{roomID}/{userID}
type RespMakeJoin struct {
	// An incomplete m.room.member event for a user on the requesting server
	// generated by the responding server.
	// See https://matrix.org/docs/spec/server_server/unstable.html#joining-rooms
	JoinEvent EventBuilder `json:"event"`
}

// A RespSendJoin is the content of a response to PUT /_matrix/federation/v2/send_join/{roomID}/{eventID}
type RespSendJoin struct {
	RespState
	Origin ServerName
}

// MarshalJSON implements json.Marshaller
func (r RespSendJoin) MarshalJSON() ([]byte, error) {
	return json.Marshal(respSendJoinFields{
		StateEvents: r.StateEvents,
		AuthEvents:  r.AuthEvents,
		Origin:      r.Origin,
	})
}

// UnmarshalJSON implements json.Unmarshaller
func (r *RespSendJoin) UnmarshalJSON(data []byte) error {
	var fields respSendJoinFields
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*r = RespSendJoin{
		Origin: fields.Origin,
		RespState: RespState{
			StateEvents: fields.StateEvents,
			AuthEvents:  fields.AuthEvents,
		},
	}
	return nil
}

type respSendJoinFields struct {
	StateEvents []Event    `json:"state"`
	AuthEvents  []Event    `json:"auth_chain"`
	Origin      ServerName `json:"origin"`
}

// ToRespState returns a new RespState with the same data from the given RespSendJoin
func (r RespSendJoin) ToRespState() RespState {
	return RespState{
		StateEvents: r.StateEvents,
		AuthEvents:  r.AuthEvents,
	}
}

// Check that a response to /send_join is valid.
// This checks that it would be valid as a response to /state
// This also checks that the join event is allowed by the state.
func (r RespSendJoin) Check(ctx context.Context, keyRing JSONVerifier, joinEvent Event) error {
	// First check that the state is valid and that the events in the response
	// are correctly signed.
	//
	// The response to /send_join has the same data as a response to /state
	// and the checks for a response to /state also apply.
	if err := r.ToRespState().Check(ctx, keyRing); err != nil {
		return err
	}

	stateEventsByID := map[string]*Event{}
	authEvents := NewAuthEvents(nil)
	for i, event := range r.StateEvents {
		stateEventsByID[event.EventID()] = &r.StateEvents[i]
		if err := authEvents.AddEvent(&r.StateEvents[i]); err != nil {
			return err
		}
	}

	// Now check that the join event is valid against its auth events.
	if err := checkAllowedByAuthEvents(joinEvent, stateEventsByID); err != nil {
		return err
	}

	// Now check that the join event is valid against the supplied state.
	if err := Allowed(joinEvent, &authEvents); err != nil {
		return fmt.Errorf(
			"gomatrixserverlib: event with ID %q is not allowed by the supplied state: %s",
			joinEvent.EventID(), err.Error(),
		)

	}

	return nil
}

// A RespMakeLeave is the content of a response to GET /_matrix/federation/v2/make_leave/{roomID}/{userID}
type RespMakeLeave struct {
	// An incomplete m.room.member event for a user on the requesting server
	// generated by the responding server.
	// See https://matrix.org/docs/spec/server_server/r0.1.1.html#get-matrix-federation-v1-make-leave-roomid-userid
	LeaveEvent EventBuilder `json:"event"`
}

// A RespDirectory is the content of a response to GET  /_matrix/federation/v1/query/directory
// This is returned when looking up a room alias from a remote server.
// See https://matrix.org/docs/spec/server_server/unstable.html#directory
type RespDirectory struct {
	// The matrix room ID the room alias corresponds to.
	RoomID string `json:"room_id"`
	// A list of matrix servers that the directory server thinks could be used
	// to join the room. The joining server may need to try multiple servers
	// before it finds one that it can use to join the room.
	Servers []ServerName `json:"servers"`
}

// RespProfile is the content of a response to GET /_matrix/federation/v1/query/profile
type RespProfile struct {
	DisplayName string `json:"displayname,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

func checkAllowedByAuthEvents(event Event, eventsByID map[string]*Event) error {
	authEvents := NewAuthEvents(nil)
	for _, authRef := range event.AuthEvents() {
		authEvent := eventsByID[authRef.EventID]
		if authEvent == nil {
			return fmt.Errorf(
				"gomatrixserverlib: missing auth event with ID %q for event %q",
				authRef.EventID, event.EventID(),
			)
		}
		if err := authEvents.AddEvent(authEvent); err != nil {
			return err
		}
	}
	if err := Allowed(event, &authEvents); err != nil {
		return fmt.Errorf(
			"gomatrixserverlib: event with ID %q is not allowed by its auth_events: %s",
			event.EventID(), err.Error(),
		)
	}
	return nil
}

// RespInvite is the content of a response to PUT /_matrix/federation/v1/invite/{roomID}/{eventID}
type RespInvite struct {
	// The invite event signed by recipient server.
	Event Event
}

// MarshalJSON implements json.Marshaller
func (r RespInvite) MarshalJSON() ([]byte, error) {
	// The wire format of a RespInvite is slightly is sent as the second element
	// of a two element list where the first element is the constant integer 200.
	// (This protocol oddity is the result of a typo in the synapse matrix
	//  server, and is preserved to maintain compatibility.)
	return json.Marshal([]interface{}{200, respInviteFields(r)})
}

// UnmarshalJSON implements json.Unmarshaller
func (r *RespInvite) UnmarshalJSON(data []byte) error {
	var tuple []RawJSON
	if err := json.Unmarshal(data, &tuple); err != nil {
		return err
	}
	if len(tuple) != 2 {
		return fmt.Errorf("gomatrixserverlib: invalid invite response, invalid length: %d != 2", len(tuple))
	}
	var fields respInviteFields
	if err := json.Unmarshal(tuple[1], &fields); err != nil {
		return err
	}
	*r = RespInvite(fields)
	return nil
}

type respInviteFields struct {
	Event Event `json:"event"`
}
