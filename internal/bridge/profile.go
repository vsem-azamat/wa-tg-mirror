package bridge

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow/types"
)

// SyncProfiles gives every person's bot their configured name and current WhatsApp photo.
func (b *Bridge) SyncProfiles(ctx context.Context, pics Pictures) {
	for _, p := range b.byPhone {
		if prof, ok := p.bot.(Profile); ok {
			if err := prof.SetName(ctx, p.botName); err != nil {
				b.log.Warn("set bot name", "person", p.name, "err", err)
			}
		}
		if err := b.syncPhoto(ctx, pics, p); err != nil {
			b.log.Warn("sync bot photo", "person", p.name, "err", err)
		}
	}
}

// PictureChanged updates the bot photo of a person whose WhatsApp picture changed.
func (b *Bridge) PictureChanged(ctx context.Context, pics Pictures, jid types.JID) {
	p, ok := b.byPhone[b.canonical(ctx, jid).User]
	if !ok {
		return
	}
	if err := b.syncPhoto(ctx, pics, p); err != nil {
		b.log.Warn("sync bot photo", "person", p.name, "err", err)
	}
}

func (b *Bridge) syncPhoto(ctx context.Context, pics Pictures, p *person) error {
	prof, ok := p.bot.(Profile)
	if !ok {
		return nil
	}
	id, jpeg, err := pics.Picture(ctx, p.jid)
	if err != nil {
		return fmt.Errorf("fetch WhatsApp picture: %w", err)
	}
	have, err := b.store.Avatar(ctx, p.bot.ID())
	if err != nil {
		return err
	}
	if have == id {
		return nil
	}
	if err := prof.SetPhoto(ctx, jpeg); err != nil {
		return err
	}
	return b.store.PutAvatar(ctx, p.bot.ID(), id)
}
