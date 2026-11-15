package wa

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// Directory resolves names and phone numbers from the whatsmeow store.
type Directory struct {
	Client *whatsmeow.Client
}

// Own returns the account's own phone-number JID.
func (d Directory) Own() types.JID {
	if d.Client.Store.ID == nil {
		return types.EmptyJID
	}
	return d.Client.Store.ID.ToNonAD()
}

// IsOwn reports whether jid is the account's own phone-number JID or LID.
func (d Directory) IsOwn(jid types.JID) bool {
	jid = jid.ToNonAD()
	if id := d.Client.Store.ID; id != nil && jid.User == id.User && jid.Server == types.DefaultUserServer {
		return true
	}
	lid := d.Client.Store.GetLID()
	return !lid.IsEmpty() && jid.User == lid.User && jid.Server == types.HiddenUserServer
}

// PhoneJID returns the phone-number JID of a LID, or jid when it is not a LID or is unknown.
func (d Directory) PhoneJID(ctx context.Context, jid types.JID) types.JID {
	if jid.Server != types.HiddenUserServer {
		return jid
	}
	pn, err := d.Client.Store.LIDs.GetPNForLID(ctx, jid)
	if err != nil || pn.IsEmpty() {
		return jid
	}
	return pn
}

// Name returns the saved contact name, the pushed name or the group subject.
func (d Directory) Name(ctx context.Context, jid types.JID) string {
	if jid.Server == types.GroupServer {
		info, err := d.Client.GetGroupInfo(ctx, jid)
		if err != nil {
			return ""
		}
		return info.Name
	}
	c, err := d.Client.Store.Contacts.GetContact(ctx, jid)
	if err != nil {
		return ""
	}
	return cmp.Or(c.FullName, c.FirstName, c.PushName, c.BusinessName)
}

// Picture returns the ID and image of a user's current profile picture, or "" and
// nil when they have none or hide it.
func (d Directory) Picture(ctx context.Context, jid types.JID) (string, []byte, error) {
	info, err := d.Client.GetProfilePictureInfo(ctx, jid, &whatsmeow.GetProfilePictureParams{})
	if errors.Is(err, whatsmeow.ErrProfilePictureNotSet) || errors.Is(err, whatsmeow.ErrProfilePictureUnauthorized) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("picture info: %w", err)
	}
	if info == nil {
		return "", nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		return "", nil, fmt.Errorf("picture request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("download picture: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("download picture: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPicture))
	if err != nil {
		return "", nil, fmt.Errorf("download picture: %w", err)
	}
	return info.ID, data, nil
}

// maxPicture caps a downloaded profile picture.
const maxPicture = 5 << 20
