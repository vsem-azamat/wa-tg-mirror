package wa

import (
	"context"
	"errors"
	"fmt"

	"go.mau.fi/whatsmeow"
)

// ErrPairingFailed means the QR pairing ended without linking the device.
var ErrPairingFailed = errors.New("whatsapp pairing failed")

// Connect connects the client. A client without a session is paired first: every
// QR code to scan is passed to onQR until the phone links the device.
func Connect(ctx context.Context, cli *whatsmeow.Client, onQR func(code string)) error {
	if cli.Store.ID != nil {
		if err := cli.Connect(); err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		return nil
	}
	qrs, err := cli.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("qr channel: %w", err)
	}
	if err := cli.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	for item := range qrs {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			onQR(item.Code)
		case whatsmeow.QRChannelSuccess.Event:
			return nil
		case whatsmeow.QRChannelEventPasskeyRequest, whatsmeow.QRChannelEventPasskeyResponse:
			// Passkey pairing is not used; the QR flow continues.
		case whatsmeow.QRChannelEventError:
			return fmt.Errorf("%w: %w", ErrPairingFailed, item.Error)
		default:
			cli.Disconnect()
			return fmt.Errorf("%w: %s", ErrPairingFailed, item.Event)
		}
	}
	return ErrPairingFailed
}
