package contentmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/application-research/estuary/collections"
	"github.com/application-research/estuary/constants"
	"github.com/application-research/estuary/model"
	"github.com/application-research/estuary/pinner/operation"
	"github.com/application-research/estuary/pinner/progress"
	"github.com/application-research/estuary/pinner/types"
	"github.com/application-research/estuary/util"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-merkledag"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/pkg/errors"
	"golang.org/x/xerrors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (cm *ContentManager) PinStatus(cont util.Content, origins []*peer.AddrInfo) (*types.IpfsPinStatusResponse, error) {
	delegates := cm.PinDelegatesForContent(cont)

	meta := make(map[string]interface{}, 0)
	if cont.PinMeta != "" {
		if err := json.Unmarshal([]byte(cont.PinMeta), &meta); err != nil {
			cm.log.Warnf("content %d has invalid pinmeta: %s", cont, err)
		}
	}

	originStrs := make([]string, 0)
	for _, o := range origins {
		ai, err := peer.AddrInfoToP2pAddrs(o)
		if err == nil {
			for _, a := range ai {
				originStrs = append(originStrs, a.String())
			}
		}
	}

	ps := &types.IpfsPinStatusResponse{
		RequestID: fmt.Sprintf("%d", cont.ID),
		Status:    types.GetContentPinningStatus(cont),
		Created:   cont.CreatedAt,
		Pin: types.IpfsPin{
			CID:     cont.Cid.CID.String(),
			Name:    cont.Name,
			Meta:    meta,
			Origins: originStrs,
		},
		Content:   cont,
		Delegates: delegates,
		Info:      make(map[string]interface{}, 0), // TODO: all sorts of extra info we could add...
	}
	return ps, nil
}

func (cm *ContentManager) PinDelegatesForContent(cont util.Content) []string {
	out := make([]string, 0)

	if cont.Location == constants.ContentLocationLocal {
		for _, a := range cm.node.Host.Addrs() {
			out = append(out, fmt.Sprintf("%s/p2p/%s", a, cm.node.Host.ID()))
		}
		return out
	}

	ai, err := cm.addrInfoForContentLocation(cont.Location)
	if err != nil {
		cm.log.Warnf("failed to get address info for shuttle %q: %s", cont.Location, err)
		return out
	}

	if ai == nil {
		cm.log.Warnf("no address info for shuttle: %s", cont.Location)
		return out
	}

	for _, a := range ai.Addrs {
		out = append(out, fmt.Sprintf("%s/p2p/%s", a, ai.ID))
	}
	return out
}

func (cm *ContentManager) PinContent(ctx context.Context, user uint, obj cid.Cid, filename string, cols []*collections.CollectionRef, origins []*peer.AddrInfo, replaceID uint, meta map[string]interface{}, replication int, makeDeal bool) (*types.IpfsPinStatusResponse, *operation.PinningOperation, error) {
	if replaceID > 0 {
		// mark as replace since it will removed and so it should not be fetched anymore
		if err := cm.db.Model(&util.Content{}).Where("id = ?", replaceID).Update("replace", true).Error; err != nil {
			return nil, nil, err
		}
	}

	var metaStr string
	if meta != nil {
		b, err := json.Marshal(meta)
		if err != nil {
			return nil, nil, err
		}
		metaStr = string(b)
	}

	var originsStr string
	if origins != nil {
		b, err := json.Marshal(origins)
		if err != nil {
			return nil, nil, err
		}
		originsStr = string(b)
	}

	loc, err := cm.shuttleMgr.GetLocationForStorage(ctx, obj, user)
	if err != nil {
		return nil, nil, xerrors.Errorf("selecting location for content failed: %w", err)
	}

	cont := util.Content{
		Cid:         util.DbCID{CID: obj},
		Name:        filename,
		UserID:      user,
		Active:      false,
		Replication: replication,
		Pinning:     false,
		PinMeta:     metaStr,
		Location:    loc,
		Origins:     originsStr,
	}
	if err := cm.db.Create(&cont).Error; err != nil {
		return nil, nil, err
	}

	if len(cols) > 0 {
		for _, c := range cols {
			c.Content = cont.ID
		}

		if err := cm.db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "path"}, {Name: "collection"}},
			DoUpdates: clause.AssignmentColumns([]string{"created_at", "content"}),
		}).Create(cols).Error; err != nil {
			return nil, nil, err
		}
	}

	var pinOp *operation.PinningOperation
	if loc == constants.ContentLocationLocal {
		pinOp = cm.GetPinOperation(cont, origins, replaceID, makeDeal)
	} else {
		if err := cm.shuttleMgr.PinContent(ctx, loc, cont, origins); err != nil {
			return nil, nil, err
		}
	}

	ipfsRes, err := cm.PinStatus(cont, origins)
	if err != nil {
		return nil, nil, err
	}
	return ipfsRes, pinOp, nil
}

func (cm *ContentManager) GetPinOperation(cont util.Content, peers []*peer.AddrInfo, replaceID uint, makeDeal bool) *operation.PinningOperation {
	if cont.Location != constants.ContentLocationLocal {
		cm.log.Errorf("calling addPinToQueue on non-local content")
	}

	return &operation.PinningOperation{
		ContId:   cont.ID,
		UserId:   cont.UserID,
		Obj:      cont.Cid.CID,
		Name:     cont.Name,
		Peers:    operation.SerializePeers(peers),
		Started:  cont.CreatedAt,
		Status:   types.GetContentPinningStatus(cont),
		Replace:  replaceID,
		Location: cont.Location,
		MakeDeal: makeDeal,
		Meta:     cont.PinMeta,
	}
}

// UpdateContentPinStatus updates content pinning statuses in DB and removes the content from its zone if failed
func (cm *ContentManager) UpdateContentPinStatus(contID uint64, location string, status types.PinningStatus) error {
	cm.log.Debugf("updating pin: %d, status: %s, loc: %s", contID, status, location)

	var c util.Content
	if err := cm.db.First(&c, "id = ?", contID).Error; err != nil {
		return errors.Wrap(err, "failed to look up content")
	}

	// if an aggregate zone is failing, zone is stuck
	// TODO - revisit this later if it is actually happening
	if c.Aggregate && status == types.PinningStatusFailed {
		cm.log.Warnf("zone: %d is stuck, failed to aggregate(pin) on location: %s", c.ID, location)

		return cm.db.Model(model.StagingZone{}).Where("id = ?", contID).UpdateColumns(map[string]interface{}{
			"status":  model.ZoneStatusStuck,
			"message": model.ZoneMessageStuck,
		}).Error
	}

	updates := map[string]interface{}{
		"active":  status == types.PinningStatusPinned,
		"pinning": status == types.PinningStatusPinning,
		"failed":  status == types.PinningStatusFailed,
	}

	if status == types.PinningStatusFailed {
		updates["aggregated_in"] = 0 // remove from staging zone so the zone can consolidate without it
	}

	return cm.db.Transaction(func(tx *gorm.DB) error {
		if err := cm.db.Model(util.Content{}).Where("id = ?", contID).UpdateColumns(updates).Error; err != nil {
			cm.log.Errorf("failed to update content status as %s in database: %s", status, err)
			return err
		}

		// deduct from the zone, so new content can be added, this way we get consistent size for aggregation
		// we did not reset the flag so that consolidation will not be reattempted by the worker
		if c.AggregatedIn > 0 {
			return tx.Raw("UPDATE staging_zones SET size = size - ? WHERE cont_id = ? ", c.Size, contID).Error
		}
		return nil
	})
}

func (cm *ContentManager) DoPinning(ctx context.Context, op *operation.PinningOperation, cb progress.PinProgressCB) error {
	ctx, span := cm.tracer.Start(ctx, "doPinning")
	defer span.End()

	// remove replacement async - move this out
	if op.Replace > 0 {
		go func() {
			if err := cm.RemoveContent(ctx, op.Replace, true); err != nil {
				cm.log.Infof("failed to remove content in replacement: %d with: %d", op.Replace, op.ContId)
			}
		}()
	}

	var c util.Content
	if err := cm.db.First(&c, "id = ?", op.ContId).Error; err != nil {
		return errors.Wrap(err, "failed to look up content for dopinning")
	}

	prs := operation.UnSerializePeers(op.Peers)
	for _, pi := range prs {
		if err := cm.node.Host.Connect(ctx, *pi); err != nil {
			cm.log.Warnf("failed to connect to origin node for pinning operation: %s", err)
		}
	}

	bserv := blockservice.New(cm.node.Blockstore, cm.node.Bitswap)
	dserv := merkledag.NewDAGService(bserv)
	dsess := dserv.Session(ctx)

	cntSize, err := cm.AddDatabaseTrackingToContent(ctx, op.ContId, dsess, op.Obj, cb)
	if err != nil {
		return err
	}

	if op.MakeDeal {
		cm.ToCheck(op.ContId, cntSize)
	}

	// this provide call goes out immediately
	if err := cm.node.FullRT.Provide(ctx, op.Obj, true); err != nil {
		cm.log.Warnf("provider broadcast failed: %s", err)
	}

	// this one adds to a queue
	if err := cm.node.Provider.Provide(op.Obj); err != nil {
		cm.log.Warnf("providing failed: %s", err)
	}
	return nil
}
