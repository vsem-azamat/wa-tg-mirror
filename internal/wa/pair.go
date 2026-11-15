package wa

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"google.golang.org/protobuf/proto"
)

// fullSyncDays is how far back a full history sync is asked for; WhatsApp caps it.
const fullSyncDays = 35

// RequestFullHistory makes the next pairing ask the phone for the full chat history
// of the last days instead of only the recent messages. It changes the device
// properties of the whole process and must be called before pairing.
func RequestFullHistory(days int) {
	store.DeviceProps.RequireFullSync = proto.Bool(true)
	store.DeviceProps.HistorySyncConfig.FullSyncDaysLimit = proto.Uint32(uint32(min(max(days, 1), fullSyncDays)))
}

// PairWithCode links a client without a session by a pairing code for phone: the
// code is passed to onCode, to be typed in WhatsApp → Linked devices → Link with
// phone number. It returns once the phone has linked the device.
func PairWithCode(ctx context.Context, cli *whatsmeow.Client, phone string, onCode func(code string)) error {
	if cli.Store.ID != nil {
		return fmt.Errorf("%w: the device is already linked", ErrPairingFailed)
	}
	qrs, err := cli.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("qr channel: %w", err)
	}
	if err := cli.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	asked := false
	for item := range qrs {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			// The first QR code means the socket is ready for a pairing code request.
			if !asked {
				asked = true
				code, err := cli.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
				if err != nil {
					cli.Disconnect()
					return fmt.Errorf("%w: request pairing code: %w", ErrPairingFailed, err)
				}
				onCode(code)
			}
		case whatsmeow.QRChannelSuccess.Event:
			return nil
		case whatsmeow.QRChannelEventPasskeyRequest, whatsmeow.QRChannelEventPasskeyResponse:
		case whatsmeow.QRChannelEventError:
			return fmt.Errorf("%w: %w", ErrPairingFailed, item.Error)
		default:
			cli.Disconnect()
			return fmt.Errorf("%w: %s", ErrPairingFailed, item.Event)
		}
	}
	return ErrPairingFailed
}
