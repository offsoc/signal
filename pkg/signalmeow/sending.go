// mautrix-signal - A Matrix-signal puppeting bridge.
// Copyright (C) 2023 Scott Weber
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package signalmeow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.mau.fi/util/exfmt"
	"go.mau.fi/util/ptr"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
	signalpb "go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/types"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/web"
)

// Sending

func (cli *Client) senderCertificate(ctx context.Context, e164 bool) (*libsignalgo.SenderCertificate, error) {
	cached := cli.SenderCertificateNoE164
	if e164 {
		cached = cli.SenderCertificateWithE164
	}
	setCache := func(val *libsignalgo.SenderCertificate) {
		if e164 {
			cli.SenderCertificateWithE164 = val
		} else {
			cli.SenderCertificateNoE164 = val
		}
	}
	if cached != nil {
		expiry, err := cached.GetExpiration()
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to check sender certificate expiry")
		} else if time.Until(expiry) < 1*exfmt.Day {
			zerolog.Ctx(ctx).Debug().Msg("Sender certificate expired, fetching new one")
			setCache(nil)
		} else {
			return cached, nil
		}
	}

	type response struct {
		Certificate []byte `json:"certificate"`
	}
	var r response

	username, password := cli.Store.BasicAuthCreds()
	opts := &web.HTTPReqOpt{Username: &username, Password: &password}
	var query string
	if !e164 {
		query = "?includeE164=false"
	}
	resp, err := web.SendHTTPRequest(ctx, http.MethodGet, "/v1/certificate/delivery"+query, opts)
	if err != nil {
		return nil, err
	}
	err = web.DecodeHTTPResponseBody(ctx, &r, resp)
	if err != nil {
		return nil, err
	}

	cert, err := libsignalgo.DeserializeSenderCertificate(r.Certificate)
	setCache(cert)
	return cert, err
}

type MyMessage struct {
	Type                      int    `json:"type"`
	DestinationDeviceID       int    `json:"destinationDeviceId"`
	DestinationRegistrationID int    `json:"destinationRegistrationId"`
	Content                   string `json:"content"`
}

type MyMessages struct {
	Timestamp uint64      `json:"timestamp"`
	Online    bool        `json:"online"`
	Urgent    bool        `json:"urgent"`
	Messages  []MyMessage `json:"messages"`
}

func padBlock(block *[]byte, pos int) error {
	if pos >= len(*block) {
		return errors.New("Padding error: position exceeds block length")
	}

	(*block)[pos] = 0x80
	for i := pos + 1; i < len(*block); i++ {
		(*block)[i] = 0
	}

	return nil
}

func addPadding(version uint32, contents []byte) ([]byte, error) {
	if version < 2 {
		return nil, fmt.Errorf("Unknown version %d", version)
	} else if version == 2 {
		return contents, nil
	} else {
		messageLength := len(contents)
		messageLengthWithTerminator := len(contents) + 1
		messagePartCount := messageLengthWithTerminator / 160
		if messageLengthWithTerminator%160 != 0 {
			messagePartCount++
		}

		messageLengthWithPadding := messagePartCount * 160

		buffer := make([]byte, messageLengthWithPadding)
		copy(buffer[:messageLength], contents)

		err := padBlock(&buffer, messageLength)
		if err != nil {
			return nil, fmt.Errorf("Invalid message padding: %w", err)
		}
		return buffer, nil
	}
}

func checkForErrorWithSessions(err error, addresses []*libsignalgo.Address, sessionRecords []*libsignalgo.SessionRecord) error {
	if err != nil {
		return err
	}
	if addresses == nil || sessionRecords == nil {
		return fmt.Errorf("addresses or session records are nil")
	}
	if len(addresses) != len(sessionRecords) {
		return fmt.Errorf("mismatched number of addresses (%d) and session records (%d)", len(addresses), len(sessionRecords))
	}
	if len(addresses) == 0 || len(sessionRecords) == 0 {
		return fmt.Errorf("no addresses or session records")
	}
	return nil
}

func (cli *Client) howManyOtherDevicesDoWeHave(ctx context.Context) int {
	addresses, _, err := cli.Store.ACISessionStore.AllSessionsForServiceID(ctx, cli.Store.ACIServiceID())
	if err != nil {
		return 0
	}
	// Filter out our deviceID
	otherDevices := 0
	for _, address := range addresses {
		deviceID, err := address.DeviceID()
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Error getting deviceID from address")
			continue
		}
		if deviceID != uint(cli.Store.DeviceID) {
			otherDevices++
		}
	}
	return otherDevices
}

func (cli *Client) buildMessagesToSend(ctx context.Context, recipient libsignalgo.ServiceID, content *signalpb.Content, unauthenticated, isGroup bool) ([]MyMessage, error) {
	// We need to prevent multiple encryption operations from happening at once, or else ratchets can race
	cli.encryptionLock.Lock()
	defer cli.encryptionLock.Unlock()

	addresses, sessionRecords, err := cli.Store.ACISessionStore.AllSessionsForServiceID(ctx, recipient)
	if err == nil && (len(addresses) == 0 || len(sessionRecords) == 0) {
		// No sessions, make one with prekey
		err = cli.FetchAndProcessPreKey(ctx, recipient, -1)
		if err != nil {
			return nil, err
		}
		addresses, sessionRecords, err = cli.Store.ACISessionStore.AllSessionsForServiceID(ctx, recipient)
	}
	err = checkForErrorWithSessions(err, addresses, sessionRecords)
	if err != nil {
		return nil, err
	}

	messages := make([]MyMessage, 0, len(addresses))
	for i, recipientAddress := range addresses {
		recipientDeviceID, err := recipientAddress.DeviceID()
		if err != nil {
			return nil, err
		}

		// Don't send to this device that we are sending from
		if recipient == cli.Store.ACIServiceID() && recipientDeviceID == uint(cli.Store.DeviceID) {
			zerolog.Ctx(ctx).Debug().
				Uint("recipient_device_id", recipientDeviceID).
				Msg("Not sending to the device I'm sending from")
			continue
		}

		// Build message payload
		serializedMessage, err := proto.Marshal(content)
		if err != nil {
			return nil, err
		}
		paddedMessage, err := addPadding(3, serializedMessage) // TODO: figure out how to get actual version
		if err != nil {
			return nil, err
		}
		sessionRecord := sessionRecords[i]

		var envelopeType int
		var encryptedPayload []byte
		if unauthenticated {
			includeE164 := !isGroup && cli.Store.AccountRecord.GetPhoneNumberSharingMode() == signalpb.AccountRecord_EVERYBODY
			envelopeType, encryptedPayload, err = cli.buildSSMessageToSend(
				ctx, recipientAddress, paddedMessage, getContentHint(content), includeE164,
			)
		} else {
			envelopeType, encryptedPayload, err = cli.buildAuthedMessageToSend(ctx, recipientAddress, paddedMessage)
		}
		if err != nil {
			return nil, err
		}

		destinationRegistrationID, err := sessionRecord.GetRemoteRegistrationID()
		if err != nil {
			return nil, err
		}
		outgoingMessage := MyMessage{
			Type:                      envelopeType,
			DestinationDeviceID:       int(recipientDeviceID),
			DestinationRegistrationID: int(destinationRegistrationID),
			Content:                   base64.StdEncoding.EncodeToString(encryptedPayload),
		}
		messages = append(messages, outgoingMessage)
	}

	return messages, nil
}

func (cli *Client) buildAuthedMessageToSend(ctx context.Context, recipientAddress *libsignalgo.Address, paddedMessage []byte) (envelopeType int, encryptedPayload []byte, err error) {
	cipherTextMessage, err := libsignalgo.Encrypt(
		ctx,
		paddedMessage,
		recipientAddress,
		cli.Store.ACISessionStore,
		cli.Store.ACIIdentityStore,
	)
	if err != nil {
		return 0, nil, err
	}
	encryptedPayload, err = cipherTextMessage.Serialize()
	if err != nil {
		return 0, nil, err
	}

	// OMG Signal are you serious why can't your magic numbers just align
	cipherMessageType, _ := cipherTextMessage.MessageType()
	if cipherMessageType == libsignalgo.CiphertextMessageTypePreKey { // 3 -> 3
		envelopeType = int(signalpb.Envelope_PREKEY_BUNDLE)
	} else if cipherMessageType == libsignalgo.CiphertextMessageTypeWhisper { // 2 -> 1
		envelopeType = int(signalpb.Envelope_CIPHERTEXT)
	} else {
		return 0, nil, fmt.Errorf("unknown message type: %v", cipherMessageType)
	}
	return envelopeType, encryptedPayload, nil
}

func (cli *Client) buildSSMessageToSend(ctx context.Context, recipientAddress *libsignalgo.Address, paddedMessage []byte, contentHint libsignalgo.UnidentifiedSenderMessageContentHint, includeE164 bool) (envelopeType int, encryptedPayload []byte, err error) {
	cert, err := cli.senderCertificate(ctx, includeE164)
	if err != nil {
		return 0, nil, err
	}
	encryptedPayload, err = libsignalgo.SealedSenderEncryptPlaintext(
		ctx,
		paddedMessage,
		contentHint,
		recipientAddress,
		cert,
		cli.Store.ACISessionStore,
		cli.Store.ACIIdentityStore,
	)
	if err != nil {
		return 0, nil, err
	}
	envelopeType = int(signalpb.Envelope_UNIDENTIFIED_SENDER)

	return envelopeType, encryptedPayload, nil
}

type SuccessfulSendResult struct {
	Recipient                 libsignalgo.ServiceID
	RecipientE164             *string
	Unidentified              bool
	DestinationPNIIdentityKey *libsignalgo.IdentityKey
}
type FailedSendResult struct {
	Recipient libsignalgo.ServiceID
	Error     error
}
type SendMessageResult struct {
	WasSuccessful bool
	SuccessfulSendResult
	FailedSendResult
}
type GroupMessageSendResult struct {
	SuccessfullySentTo []SuccessfulSendResult
	FailedToSendTo     []FailedSendResult
}

type SendResult interface {
	isSendResult()
}

func (gmsr *GroupMessageSendResult) isSendResult() {}
func (smsr *SendMessageResult) isSendResult()      {}

func contentFromDataMessage(dataMessage *signalpb.DataMessage) *signalpb.Content {
	return &signalpb.Content{
		DataMessage: dataMessage,
	}
}
func syncMessageFromGroupDataMessage(dataMessage *signalpb.DataMessage, results []SuccessfulSendResult) *signalpb.Content {
	unidentifiedStatuses := []*signalpb.SyncMessage_Sent_UnidentifiedDeliveryStatus{}
	for _, result := range results {
		unidentifiedStatuses = append(unidentifiedStatuses, &signalpb.SyncMessage_Sent_UnidentifiedDeliveryStatus{
			DestinationServiceId: proto.String(result.Recipient.String()),
			Unidentified:         &result.Unidentified,
		})
	}
	return &signalpb.Content{
		SyncMessage: &signalpb.SyncMessage{
			Sent: &signalpb.SyncMessage_Sent{
				Message:                  dataMessage,
				Timestamp:                dataMessage.Timestamp,
				UnidentifiedStatus:       unidentifiedStatuses,
				ExpirationStartTimestamp: ptr.Ptr(uint64(time.Now().UnixMilli())),
			},
		},
	}
}
func syncMessageFromGroupEditMessage(editMessage *signalpb.EditMessage, results []SuccessfulSendResult) *signalpb.Content {
	unidentifiedStatuses := []*signalpb.SyncMessage_Sent_UnidentifiedDeliveryStatus{}
	for _, result := range results {
		unidentifiedStatuses = append(unidentifiedStatuses, &signalpb.SyncMessage_Sent_UnidentifiedDeliveryStatus{
			DestinationServiceId: proto.String(result.Recipient.String()),
			Unidentified:         &result.Unidentified,
		})
	}
	return &signalpb.Content{
		SyncMessage: &signalpb.SyncMessage{
			Sent: &signalpb.SyncMessage_Sent{
				EditMessage:              editMessage,
				Timestamp:                editMessage.GetDataMessage().Timestamp,
				UnidentifiedStatus:       unidentifiedStatuses,
				ExpirationStartTimestamp: ptr.Ptr(uint64(time.Now().UnixMilli())),
			},
		},
	}
}

func syncMessageFromSoloDataMessage(dataMessage *signalpb.DataMessage, result SuccessfulSendResult) *signalpb.Content {
	return &signalpb.Content{
		SyncMessage: &signalpb.SyncMessage{
			Sent: &signalpb.SyncMessage_Sent{
				Message:                  dataMessage,
				DestinationE164:          result.RecipientE164,
				DestinationServiceId:     proto.String(result.Recipient.String()),
				Timestamp:                dataMessage.Timestamp,
				ExpirationStartTimestamp: ptr.Ptr(uint64(time.Now().UnixMilli())),
				UnidentifiedStatus: []*signalpb.SyncMessage_Sent_UnidentifiedDeliveryStatus{
					{
						DestinationServiceId:      proto.String(result.Recipient.String()),
						Unidentified:              &result.Unidentified,
						DestinationPniIdentityKey: result.DestinationPNIIdentityKey.TrySerialize(),
					},
				},
			},
		},
	}
}

func syncMessageFromSoloEditMessage(editMessage *signalpb.EditMessage, result SuccessfulSendResult) *signalpb.Content {
	return &signalpb.Content{
		SyncMessage: &signalpb.SyncMessage{
			Sent: &signalpb.SyncMessage_Sent{
				EditMessage:              editMessage,
				DestinationE164:          result.RecipientE164,
				DestinationServiceId:     proto.String(result.Recipient.String()),
				Timestamp:                editMessage.DataMessage.Timestamp,
				ExpirationStartTimestamp: ptr.Ptr(uint64(time.Now().UnixMilli())),
				UnidentifiedStatus: []*signalpb.SyncMessage_Sent_UnidentifiedDeliveryStatus{
					{
						DestinationServiceId:      proto.String(result.Recipient.String()),
						Unidentified:              &result.Unidentified,
						DestinationPniIdentityKey: result.DestinationPNIIdentityKey.TrySerialize(),
					},
				},
			},
		},
	}
}

func syncMessageFromReadReceiptMessage(ctx context.Context, receiptMessage *signalpb.ReceiptMessage, messageSender libsignalgo.ServiceID) *signalpb.Content {
	if *receiptMessage.Type != signalpb.ReceiptMessage_READ {
		zerolog.Ctx(ctx).Warn().
			Any("receipt_message_type", receiptMessage.Type).
			Msg("syncMessageFromReadReceiptMessage called with non-read receipt message")
		return nil
	} else if messageSender.Type != libsignalgo.ServiceIDTypeACI {
		zerolog.Ctx(ctx).Warn().
			Stringer("message_sender", messageSender).
			Msg("syncMessageFromReadReceiptMessage called with non-ACI message sender")
		return nil
	}
	read := []*signalpb.SyncMessage_Read{}
	for _, timestamp := range receiptMessage.Timestamp {
		read = append(read, &signalpb.SyncMessage_Read{
			Timestamp: proto.Uint64(timestamp),
			SenderAci: proto.String(messageSender.UUID.String()),
		})
	}
	return &signalpb.Content{
		SyncMessage: &signalpb.SyncMessage{
			Read: read,
		},
	}
}

func (cli *Client) SendContactSyncRequest(ctx context.Context) error {
	log := zerolog.Ctx(ctx).With().
		Str("action", "send contact sync request").
		Time("last_request_time", cli.LastContactRequestTime).
		Logger()
	ctx = log.WithContext(ctx)
	// If we've requested in the last minute, don't request again
	if time.Since(cli.LastContactRequestTime) < 60*time.Second {
		log.Warn().Msg("Not sending contact sync request because we already requested it in the past minute")
		return nil
	}

	cli.LastContactRequestTime = time.Now()
	_, err := cli.sendContent(ctx, cli.Store.ACIServiceID(), uint64(time.Now().UnixMilli()), &signalpb.Content{
		SyncMessage: &signalpb.SyncMessage{
			Request: &signalpb.SyncMessage_Request{
				Type: signalpb.SyncMessage_Request_CONTACTS.Enum(),
			},
		},
	}, 0, false, false)
	if err != nil {
		log.Err(err).Msg("Failed to send contact sync request message to myself")
		return err
	}
	return nil
}

func (cli *Client) SendStorageMasterKeyRequest(ctx context.Context) error {
	log := zerolog.Ctx(ctx).With().
		Str("action", "send key sync request").
		Logger()
	ctx = log.WithContext(ctx)

	_, err := cli.sendContent(ctx, cli.Store.ACIServiceID(), uint64(time.Now().UnixMilli()), &signalpb.Content{
		SyncMessage: &signalpb.SyncMessage{
			Request: &signalpb.SyncMessage_Request{
				Type: signalpb.SyncMessage_Request_KEYS.Enum(),
			},
		},
	}, 0, false, false)
	if err != nil {
		log.Err(err).Msg("Failed to send key sync request message to myself")
		return err
	} else {
		log.Info().Msg("Sent key sync request to self")
	}
	return nil
}

func TypingMessage(isTyping bool) *signalpb.Content {
	// Note: not handling sending to a group ATM since that will require
	// SenderKey sending to not be terrible
	timestamp := currentMessageTimestamp()
	var action signalpb.TypingMessage_Action
	if isTyping {
		action = signalpb.TypingMessage_STARTED
	} else {
		action = signalpb.TypingMessage_STOPPED
	}
	tm := &signalpb.TypingMessage{
		Timestamp: &timestamp,
		Action:    &action,
	}
	return &signalpb.Content{
		TypingMessage: tm,
	}
}

func DeliveredReceiptMessageForTimestamps(timestamps []uint64) *signalpb.Content {
	rm := &signalpb.ReceiptMessage{
		Timestamp: timestamps,
		Type:      signalpb.ReceiptMessage_DELIVERY.Enum(),
	}
	return &signalpb.Content{
		ReceiptMessage: rm,
	}
}

func ReadReceptMessageForTimestamps(timestamps []uint64) *signalpb.Content {
	rm := &signalpb.ReceiptMessage{
		Timestamp: timestamps,
		Type:      signalpb.ReceiptMessage_READ.Enum(),
	}
	return &signalpb.Content{
		ReceiptMessage: rm,
	}
}

func DataMessageForReaction(reaction string, targetMessageSender uuid.UUID, targetMessageTimestamp uint64, removing bool) *signalpb.Content {
	timestamp := currentMessageTimestamp()
	dm := &signalpb.DataMessage{
		Timestamp:               &timestamp,
		RequiredProtocolVersion: proto.Uint32(uint32(signalpb.DataMessage_REACTIONS)),
		Reaction: &signalpb.DataMessage_Reaction{
			Emoji:               proto.String(reaction),
			Remove:              proto.Bool(removing),
			TargetAuthorAci:     proto.String(targetMessageSender.String()),
			TargetSentTimestamp: proto.Uint64(targetMessageTimestamp),
		},
	}
	return wrapDataMessageInContent(dm)
}

func DataMessageForDelete(targetMessageTimestamp uint64) *signalpb.Content {
	timestamp := currentMessageTimestamp()
	dm := &signalpb.DataMessage{
		Timestamp: &timestamp,
		Delete: &signalpb.DataMessage_Delete{
			TargetSentTimestamp: proto.Uint64(targetMessageTimestamp),
		},
	}
	return wrapDataMessageInContent(dm)
}

func wrapDataMessageInContent(dm *signalpb.DataMessage) *signalpb.Content {
	return &signalpb.Content{
		DataMessage: dm,
	}
}

func (cli *Client) SendGroupUpdate(ctx context.Context, group *Group, groupContext *signalpb.GroupContextV2, groupChange *GroupChange) (*GroupMessageSendResult, error) {
	log := zerolog.Ctx(ctx).With().
		Str("action", "send group change message").
		Stringer("group_id", group.GroupIdentifier).
		Logger()
	ctx = log.WithContext(ctx)
	timestamp := currentMessageTimestamp()
	dm := &signalpb.DataMessage{
		Timestamp: &timestamp,
		GroupV2:   groupContext,
	}
	content := wrapDataMessageInContent(dm)
	var recipients []*libsignalgo.ServiceID
	for _, member := range group.Members {
		serviceID := member.UserServiceID()
		recipients = append(recipients, &serviceID)
	}
	for _, member := range group.PendingMembers {
		recipients = append(recipients, &member.ServiceID)
	}
	if groupChange != nil {
		for _, member := range groupChange.AddPendingMembers {
			recipients = append(recipients, &member.ServiceID)
		}
		for _, member := range groupChange.AddMembers {
			serviceID := member.UserServiceID()
			recipients = append(recipients, &serviceID)
		}
	}
	return cli.sendToGroup(ctx, recipients, content, timestamp)
}

func (cli *Client) SendGroupMessage(ctx context.Context, gid types.GroupIdentifier, content *signalpb.Content) (*GroupMessageSendResult, error) {
	log := zerolog.Ctx(ctx).With().
		Str("action", "send group message").
		Stringer("group_id", gid).
		Logger()
	ctx = log.WithContext(ctx)
	group, err := cli.RetrieveGroupByID(ctx, gid, 0)
	if err != nil {
		return nil, err
	}

	var messageTimestamp uint64
	if content.GetDataMessage() != nil {
		messageTimestamp = content.DataMessage.GetTimestamp()
		content.DataMessage.GroupV2 = groupMetadataForDataMessage(*group)
	} else if content.GetEditMessage().GetDataMessage() != nil {
		messageTimestamp = content.EditMessage.DataMessage.GetTimestamp()
		content.EditMessage.DataMessage.GroupV2 = groupMetadataForDataMessage(*group)
	}
	var recipients []*libsignalgo.ServiceID
	for _, member := range group.Members {
		serviceID := member.UserServiceID()
		recipients = append(recipients, &serviceID)
	}
	return cli.sendToGroup(ctx, recipients, content, messageTimestamp)
}

func (cli *Client) sendToGroup(ctx context.Context, recipients []*libsignalgo.ServiceID, content *signalpb.Content, messageTimestamp uint64) (*GroupMessageSendResult, error) {
	// Send to each member of the group
	result := &GroupMessageSendResult{
		SuccessfullySentTo: []SuccessfulSendResult{},
		FailedToSendTo:     []FailedSendResult{},
	}
	for _, recipient := range recipients {
		if recipient.Type == libsignalgo.ServiceIDTypeACI && recipient.UUID == cli.Store.ACI {
			// Don't send normal DataMessages to ourselves
			continue
		}
		log := zerolog.Ctx(ctx).With().Stringer("member", *recipient).Logger()
		ctx := log.WithContext(ctx)
		sentUnidentified, err := cli.sendContent(ctx, *recipient, messageTimestamp, content, 0, true, true)
		if err != nil {
			result.FailedToSendTo = append(result.FailedToSendTo, FailedSendResult{
				Recipient: *recipient,
				Error:     err,
			})
			log.Err(err).Msg("Failed to send to user")
		} else {
			result.SuccessfullySentTo = append(result.SuccessfullySentTo, SuccessfulSendResult{
				Recipient:    *recipient,
				Unidentified: sentUnidentified,
			})
			log.Trace().Msg("Successfully sent to user")
		}
	}

	// No need to send to ourselves if we don't have any other devices
	if cli.howManyOtherDevicesDoWeHave(ctx) > 0 {
		var syncContent *signalpb.Content
		if content.GetDataMessage() != nil {
			syncContent = syncMessageFromGroupDataMessage(content.DataMessage, result.SuccessfullySentTo)
		} else if content.GetEditMessage() != nil {
			syncContent = syncMessageFromGroupEditMessage(content.EditMessage, result.SuccessfullySentTo)
		}
		_, selfSendErr := cli.sendContent(ctx, cli.Store.ACIServiceID(), messageTimestamp, syncContent, 0, true, true)
		if selfSendErr != nil {
			zerolog.Ctx(ctx).Err(selfSendErr).Msg("Failed to send sync message to myself")
		}
	}

	if len(result.FailedToSendTo) == 0 && len(result.SuccessfullySentTo) == 0 {
		return result, nil // I only sent to myself
	}
	if len(result.SuccessfullySentTo) == 0 {
		lastError := result.FailedToSendTo[len(result.FailedToSendTo)-1].Error
		return nil, fmt.Errorf("failed to send to any group members: %w", lastError)
	}

	return result, nil
}

func (cli *Client) sendSyncCopy(ctx context.Context, content *signalpb.Content, messageTS uint64, result *SuccessfulSendResult) bool {
	// If we have other devices, send Sync messages to them too
	if cli.howManyOtherDevicesDoWeHave(ctx) > 0 {
		var syncContent *signalpb.Content
		if content.GetDataMessage() != nil {
			syncContent = syncMessageFromSoloDataMessage(content.DataMessage, *result)
		} else if content.GetEditMessage() != nil {
			syncContent = syncMessageFromSoloEditMessage(content.EditMessage, *result)
		} else if content.GetReceiptMessage().GetType() == signalpb.ReceiptMessage_READ {
			syncContent = syncMessageFromReadReceiptMessage(ctx, content.ReceiptMessage, result.Recipient)
		}
		if syncContent != nil {
			_, selfSendErr := cli.sendContent(ctx, cli.Store.ACIServiceID(), messageTS, syncContent, 0, true, false)
			if selfSendErr != nil {
				zerolog.Ctx(ctx).Err(selfSendErr).Msg("Failed to send sync message to myself")
			} else {
				return true
			}
		}
	}
	return false
}

func (cli *Client) SendMessage(ctx context.Context, recipientID libsignalgo.ServiceID, content *signalpb.Content) SendMessageResult {
	// Assemble the content to send
	var messageTimestamp uint64
	if content.GetDataMessage() != nil {
		messageTimestamp = *content.DataMessage.Timestamp
	} else if content.GetEditMessage().GetDataMessage() != nil {
		messageTimestamp = *content.EditMessage.DataMessage.Timestamp
	} else {
		messageTimestamp = currentMessageTimestamp()
	}
	needsPNISignature := false
	var aci, pni uuid.UUID
	if recipientID.Type == libsignalgo.ServiceIDTypeACI {
		aci = recipientID.UUID
	} else if recipientID.Type == libsignalgo.ServiceIDTypePNI {
		pni = recipientID.UUID
	}
	recipientData, err := cli.Store.RecipientStore.LoadAndUpdateRecipient(ctx, aci, pni, func(recipientData *types.Recipient) (changed bool, err error) {
		if recipientID.Type == libsignalgo.ServiceIDTypeACI && recipientData.NeedsPNISignature {
			needsPNISignature = true
			zerolog.Ctx(ctx).Debug().
				Stringer("recipient", recipientID).
				Msg("Including PNI identity in message")
			sig, err := cli.Store.PNIIdentityKeyPair.SignAlternateIdentity(cli.Store.ACIIdentityKeyPair.GetIdentityKey())
			if err != nil {
				return false, err
			}
			recipientData.NeedsPNISignature = false
			content.PniSignatureMessage = &signalpb.PniSignatureMessage{
				Pni:       cli.Store.PNI[:],
				Signature: sig,
			}
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to get message recipient data")
	}
	// Treat needs PNI signature as "this is a message request" and don't send receipts/typing
	if needsPNISignature && (content.TypingMessage != nil || content.ReceiptMessage != nil) {
		zerolog.Ctx(ctx).Debug().Msg("Not sending typing/receipt message to recipient as needs PNI signature flag is set")
		res := SuccessfulSendResult{Recipient: recipientID}
		if content.GetReceiptMessage().GetType() == signalpb.ReceiptMessage_READ {
			// Still send sync messages for read receipts
			cli.sendSyncCopy(ctx, content, messageTimestamp, &res)
		}
		return SendMessageResult{WasSuccessful: true, SuccessfulSendResult: res}
	} else if content.TypingMessage != nil && cli.Store.DeviceData.AccountRecord != nil && !cli.Store.DeviceData.AccountRecord.GetTypingIndicators() {
		zerolog.Ctx(ctx).Debug().Msg("Not sending typing message as typing indicators are disabled")
		res := SuccessfulSendResult{Recipient: recipientID}
		return SendMessageResult{WasSuccessful: true, SuccessfulSendResult: res}
	} else if content.GetReceiptMessage().GetType() == signalpb.ReceiptMessage_READ && cli.Store.DeviceData.AccountRecord != nil && !cli.Store.DeviceData.AccountRecord.GetReadReceipts() {
		zerolog.Ctx(ctx).Debug().Msg("Not sending receipt message as read receipts are disabled")
		res := SuccessfulSendResult{Recipient: recipientID}
		// Still send sync messages for read receipts
		cli.sendSyncCopy(ctx, content, messageTimestamp, &res)
		return SendMessageResult{WasSuccessful: true, SuccessfulSendResult: res}
	}

	isDeliveryReceipt := content.ReceiptMessage != nil && content.GetReceiptMessage().GetType() == signalpb.ReceiptMessage_DELIVERY
	if recipientID == cli.Store.ACIServiceID() && !isDeliveryReceipt {
		res := SuccessfulSendResult{
			Recipient:    recipientID,
			Unidentified: false,
		}
		ok := cli.sendSyncCopy(ctx, content, messageTimestamp, &res)
		return SendMessageResult{
			WasSuccessful:        ok,
			SuccessfulSendResult: res,
		}
	}

	// Send to the recipient
	sentUnidentified, err := cli.sendContent(ctx, recipientID, messageTimestamp, content, 0, true, false)
	if err != nil {
		return SendMessageResult{
			WasSuccessful: false,
			FailedSendResult: FailedSendResult{
				Recipient: recipientID,
				Error:     err,
			},
		}
	}
	result := SendMessageResult{
		WasSuccessful: true,
		SuccessfulSendResult: SuccessfulSendResult{
			Recipient:    recipientID,
			Unidentified: sentUnidentified,
		},
	}
	if recipientID.Type == libsignalgo.ServiceIDTypePNI {
		result.DestinationPNIIdentityKey, err = cli.Store.IdentityKeyStore.GetIdentityKey(ctx, recipientID)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to add PNI destination identity key to sync message")
		}
		if recipientData != nil && recipientData.E164 != "" {
			result.RecipientE164 = &recipientData.E164
		} else {
			zerolog.Ctx(ctx).Warn().Msg("No E164 number found for PNI sync message")
		}
	}

	cli.sendSyncCopy(ctx, content, messageTimestamp, &result.SuccessfulSendResult)

	return result
}

func currentMessageTimestamp() uint64 {
	return uint64(time.Now().UnixMilli())
}

func isSyncMessageUrgent(content *signalpb.SyncMessage) bool {
	return content.Sent != nil || content.Request != nil
}

func isUrgent(content *signalpb.Content) bool {
	return content.DataMessage != nil ||
		content.CallMessage != nil ||
		content.StoryMessage != nil ||
		content.EditMessage != nil ||
		(content.SyncMessage != nil && isSyncMessageUrgent(content.SyncMessage))
}

func getContentHint(content *signalpb.Content) libsignalgo.UnidentifiedSenderMessageContentHint {
	if content.DataMessage != nil || content.EditMessage != nil {
		// TODO add support for resending before setting this
		//return libsignalgo.UnidentifiedSenderMessageContentHintResendable
	}
	if content.TypingMessage != nil || content.ReceiptMessage != nil {
		return libsignalgo.UnidentifiedSenderMessageContentHintImplicit
	}
	return libsignalgo.UnidentifiedSenderMessageContentHintDefault
}

func (cli *Client) sendContent(
	ctx context.Context,
	recipient libsignalgo.ServiceID,
	messageTimestamp uint64,
	content *signalpb.Content,
	retryCount int,
	useUnidentifiedSender bool,
	isGroup bool,
) (sentUnidentified bool, err error) {
	log := zerolog.Ctx(ctx).With().
		Str("action", "send content").
		Stringer("recipient", recipient).
		Uint64("timestamp", messageTimestamp).
		Logger()
	ctx = log.WithContext(ctx)
	log.Trace().Any("raw_content", content).Stringer("recipient", recipient).Msg("Raw data of outgoing message")

	// If it's a data message, add our profile key
	if content.DataMessage != nil {
		profileKey, err := cli.ProfileKeyForSignalID(ctx, cli.Store.ACI)
		if err != nil {
			log.Err(err).Msg("Error getting profile key, not adding to outgoing message")
		} else {
			content.DataMessage.ProfileKey = profileKey.Slice()
		}
	}

	if retryCount > 3 {
		log.Error().Int("retry_count", retryCount).Msg("sendContent too many retries")
		return false, fmt.Errorf("too many retries")
	}

	if recipient.Type == libsignalgo.ServiceIDTypePNI && recipient.UUID == cli.Store.PNI {
		return false, fmt.Errorf("can't send to own PNI")
	} else if recipient.Type == libsignalgo.ServiceIDTypeACI && recipient.UUID == cli.Store.ACI {
		// Don't use unauthed websocket to send a payload to my own other devices
		useUnidentifiedSender = false
	} else if recipient.Type == libsignalgo.ServiceIDTypePNI {
		// Can't use unidentified sender for PNIs because only ACIs have profile keys
		useUnidentifiedSender = false
	}
	var accessKey *libsignalgo.AccessKey
	if useUnidentifiedSender {
		profileKey, err := cli.ProfileKeyForSignalID(ctx, recipient.UUID)
		if err != nil {
			return false, fmt.Errorf("failed to get profile key: %w", err)
		} else if profileKey == nil {
			log.Warn().Msg("Profile key not found")
			useUnidentifiedSender = false
		} else if accessKey, err = profileKey.DeriveAccessKey(); err != nil {
			log.Err(err).Msg("Error deriving access key")
			useUnidentifiedSender = false
		}
	}

	var messages []MyMessage
	messages, err = cli.buildMessagesToSend(ctx, recipient, content, useUnidentifiedSender, isGroup)
	if err != nil {
		log.Err(err).Msg("Error building messages to send")
		return false, err
	}

	outgoingMessages := MyMessages{
		Timestamp: messageTimestamp,
		Online:    false,
		Urgent:    isUrgent(content),
		Messages:  messages,
	}
	jsonBytes, err := json.Marshal(outgoingMessages)
	if err != nil {
		return false, err
	}
	path := fmt.Sprintf("/v1/messages/%s", recipient)
	request := web.CreateWSRequest(http.MethodPut, path, jsonBytes, nil, nil)

	var response *signalpb.WebSocketResponseMessage
	if useUnidentifiedSender {
		log.Trace().Msg("Sending message over unidentified WS")
		base64AccessKey := base64.StdEncoding.EncodeToString(accessKey[:])
		request.Headers = append(request.Headers, "unidentified-access-key:"+base64AccessKey)
		response, err = cli.UnauthedWS.SendRequest(ctx, request)
	} else {
		log.Trace().Msg("Sending message over authed WS")
		response, err = cli.AuthedWS.SendRequest(ctx, request)
	}
	sentUnidentified = useUnidentifiedSender
	if err != nil {
		return sentUnidentified, err
	}
	log = log.With().
		Uint64("response_id", *response.Id).
		Uint32("response_status", *response.Status).
		Logger()
	ctx = log.WithContext(ctx)
	if json.Valid(response.GetBody()) {
		log.Debug().RawJSON("response_body", response.GetBody()).Msg("DEBUG: message send response data")
	} else {
		log.Debug().Bytes("response_body", response.GetBody()).Msg("DEBUG: message send response data")
	}
	log.Trace().Msg("Received a response to a message send")

	retryableStatuses := []uint32{409, 410, 428, 500, 503}

	// Check to see if our status is retryable
	needToRetry := false
	for _, status := range retryableStatuses {
		if *response.Status == status {
			needToRetry = true
			break
		}
	}

	if needToRetry {
		var err error
		if *response.Status == 409 {
			err = cli.handle409(ctx, recipient, response)
		} else if *response.Status == 410 {
			err = cli.handle410(ctx, recipient, response)
		} else if *response.Status == 428 {
			err = cli.handle428(ctx, recipient, response)
		}
		if err != nil {
			return false, err
		}
		// Try to send again (**RECURSIVELY**)
		sentUnidentified, err = cli.sendContent(ctx, recipient, messageTimestamp, content, retryCount+1, sentUnidentified, isGroup)
		if err != nil {
			log.Err(err).Msg("2nd try sendMessage error")
			return sentUnidentified, err
		}
	} else if *response.Status == 401 && useUnidentifiedSender {
		log.Debug().Msg("Retrying send without sealed sender")
		// Try to send again (**RECURSIVELY**)
		sentUnidentified, err = cli.sendContent(ctx, recipient, messageTimestamp, content, retryCount+1, false, isGroup)
		if err != nil {
			log.Err(err).Msg("2nd try sendMessage error")
			return sentUnidentified, err
		}
	} else if *response.Status != 200 {
		return sentUnidentified, fmt.Errorf("unexpected status code while sending: %d", *response.Status)
	}

	return sentUnidentified, nil
}

// A 409 means our device list was out of date, so we will fix it up
func (cli *Client) handle409(ctx context.Context, recipient libsignalgo.ServiceID, response *signalpb.WebSocketResponseMessage) error {
	log := zerolog.Ctx(ctx)
	// Decode json body
	// TODO use an actual struct for this
	var body map[string]interface{}
	err := json.Unmarshal(response.Body, &body)
	if err != nil {
		log.Err(err).Msg("Unmarshal error")
		return err
	}
	// check for missingDevices and extraDevices
	if body["missingDevices"] != nil {
		missingDevices := body["missingDevices"].([]any)
		log.Debug().Any("missing_devices", missingDevices).Msg("missing devices found in 409 response")
		// TODO: establish session with missing devices
		for _, missingDevice := range missingDevices {
			err = cli.FetchAndProcessPreKey(ctx, recipient, int(missingDevice.(float64)))
			if err != nil {
				return nil
			}
		}
	}
	if body["extraDevices"] != nil {
		extraDevices := body["extraDevices"].([]any)
		log.Debug().Any("extra_devices", extraDevices).Msg("extra devices found in 409 response")
		for _, extraDevice := range extraDevices {
			recipientAddr, err := recipient.Address(uint(extraDevice.(float64)))
			if err != nil {
				log.Err(err).Msg("NewAddress error")
				return err
			}
			err = cli.Store.ACISessionStore.RemoveSession(ctx, recipientAddr)
			if err != nil {
				log.Err(err).Msg("RemoveSession error")
				return err
			}
		}
	}
	return err
}

// A 410 means we have a stale device, so get rid of it
func (cli *Client) handle410(ctx context.Context, recipient libsignalgo.ServiceID, response *signalpb.WebSocketResponseMessage) error {
	log := zerolog.Ctx(ctx)
	// Decode json body
	// TODO use an actual struct
	var body map[string]interface{}
	err := json.Unmarshal(response.Body, &body)
	if err != nil {
		log.Err(err).Msg("Unmarshal error")
		return err
	}
	// check for staleDevices and make new sessions with them
	if body["staleDevices"] != nil {
		staleDevices := body["staleDevices"].([]any)
		log.Debug().Any("stale_devices", staleDevices).Msg("stale devices found in 410 response")
		for _, staleDevice := range staleDevices {
			recipientAddr, err := recipient.Address(uint(staleDevice.(float64)))
			if err != nil {
				log.Err(err).Msg("error creating new UUID Address")
				return err
			}
			err = cli.Store.ACISessionStore.RemoveSession(ctx, recipientAddr)
			if err != nil {
				log.Err(err).Msg("RemoveSession error")
				return err
			}
			err = cli.FetchAndProcessPreKey(ctx, recipient, int(staleDevice.(float64)))
			if err != nil {
				return err
			}
		}
	}
	return err
}

// We got rate limited.
// We ~~will~~ could try sending a "pushChallenge" response, but if that doesn't work we just gotta wait.
// TODO: explore captcha response
func (cli *Client) handle428(ctx context.Context, recipient libsignalgo.ServiceID, response *signalpb.WebSocketResponseMessage) error {
	log := zerolog.Ctx(ctx)
	// Decode json body
	// TODO use an actual struct
	var body map[string]interface{}
	err := json.Unmarshal(response.Body, &body)
	if err != nil {
		log.Err(err).Msg("Unmarshal error")
		return err
	}

	// Sample response:
	//id:25 status:428 message:"Precondition Required" headers:"Retry-After:86400"
	//headers:"Content-Type:application/json" headers:"Content-Length:88"
	//body:"{\"token\":\"07af0d73-e05d-42c3-9634-634922061966\",\"options\":[\"recaptcha\",\"pushChallenge\"]}"
	var retryAfterSeconds uint64 = 0
	// Find retry after header
	for _, header := range response.Headers {
		key, value := strings.Split(header, ":")[0], strings.Split(header, ":")[1]
		if key == "Retry-After" {
			retryAfterSeconds, err = strconv.ParseUint(value, 10, 64)
			if err != nil {
				log.Err(err).Msg("ParseUint error")
			}
		}
	}
	if retryAfterSeconds > 0 {
		log.Warn().Uint64("retry_after_seconds", retryAfterSeconds).Msg("Got rate limited")
	}
	// TODO: responding to a pushChallenge this way doesn't work, server just returns 422
	// Luckily challenges seem rare when sending with sealed sender
	//if body["options"] != nil {
	//	options := body["options"].([]interface{})
	//	for _, option := range options {
	//		if option == "pushChallenge" {
	//			zlog.Info().Msg("Got pushChallenge, sending response")
	//			token := body["token"].(string)
	//			username, password := device.Data.BasicAuthCreds()
	//			response, err := web.SendHTTPRequest(
	//				http.MethodPut,
	//				"/v1/challenge",
	//				&web.HTTPReqOpt{
	//					Body:     []byte(fmt.Sprintf("{\"token\":\"%v\",\"type\":\"pushChallenge\"}", token)),
	//					Username: &username,
	//					Password: &password,
	//				},
	//			)
	//			if err != nil {
	//				zlog.Err(err).Msg("SendHTTPRequest error")
	//				return err
	//			}
	//			if response.StatusCode != 200 {
	//				zlog.Info().Msg("Unexpected status code: %v", response.StatusCode)
	//				return fmt.Errorf("Unexpected status code: %v", response.StatusCode)
	//			}
	//		}
	//	}
	//}
	return nil
}
