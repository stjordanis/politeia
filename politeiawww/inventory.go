package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/davecgh/go-spew/spew"

	"github.com/decred/politeia/decredplugin"
	pd "github.com/decred/politeia/politeiad/api/v1"
	www "github.com/decred/politeia/politeiawww/api/v1"
	"github.com/decred/politeia/util"
)

type inventoryRecord struct {
	record            pd.Record                    // actual record
	proposalMD        BackendProposalMetadata      // proposal metadata
	comments          map[string]www.Comment       // [id]comment
	commentsLikes     map[string][]www.LikeComment // [id]comment likes
	changes           []MDStreamChanges            // changes metadata
	voteAuthorization www.AuthorizeVoteReply       // vote authorization metadata
	votebits          www.StartVote                // vote bits and options
	voting            www.StartVoteReply           // voting metadata
}

// proposalsRequest is used for passing parameters into the
// getProposals() function.
type proposalsRequest struct {
	After    string
	Before   string
	UserId   string
	StateMap map[www.PropStateT]bool
}

// proposalsStats is used to summarize proposal statistics
// for the full inventory and for individual users.
type proposalsStats struct {
	NumOfInvalid         int
	NumOfCensored        int
	NumOfUnvetted        int
	NumOfUnvettedChanges int
	NumOfPublic          int
	NumOfAbandoned       int
}

// _inventoryProposalStats returns the number of proposals that are in the
// inventory catagorized by proposal status.
//
// This function must be called WITH the mutex held.
func (b *backend) _inventoryProposalStats() proposalsStats {
	return proposalsStats{
		NumOfInvalid:         b.numOfInvalid,
		NumOfCensored:        b.numOfCensored,
		NumOfUnvetted:        b.numOfUnvetted,
		NumOfUnvettedChanges: b.numOfUnvettedChanges,
		NumOfPublic:          b.numOfPublic,
		NumOfAbandoned:       b.numOfAbandoned,
	}
}

// inventoryProposalStats returns the number of proposals that are in the
// inventory catagorized by proposal status.
//
// This function must be called WITHOUT the mutex held.
func (b *backend) inventoryProposalStats() proposalsStats {
	b.RLock()
	defer b.RUnlock()
	return b._inventoryProposalStats()
}

// userProposalStats returns the number of proposals for the specified user
// catagorized by proposal status.
//
// This function must be called WITHOUT the mutex held.
func (b *backend) userProposalStats(userID string) proposalsStats {
	b.RLock()
	allProposals := b._getAllProposals()
	b.RUnlock()

	var ps proposalsStats
	for _, v := range allProposals {
		if v.UserId != userID {
			continue
		}

		switch v.Status {
		case www.PropStatusInvalid:
			ps.NumOfInvalid += 1
		case www.PropStatusNotReviewed:
			ps.NumOfUnvetted += 1
		case www.PropStatusUnreviewedChanges:
			ps.NumOfUnvettedChanges += 1
		case www.PropStatusCensored:
			ps.NumOfCensored += 1
		case www.PropStatusPublic:
			ps.NumOfPublic += 1
		case www.PropStatusAbandoned:
			ps.NumOfAbandoned += 1
		}
	}

	return ps
}

// _newInventoryRecord adds a record to the inventory.
//
// This function must be called WITH the mutex held.
func (b *backend) _newInventoryRecord(record pd.Record) error {
	t := record.CensorshipRecord.Token
	if _, ok := b.inventory[t]; ok {
		return fmt.Errorf("newInventoryRecord: duplicate token: %v", t)
	}

	b.inventory[record.CensorshipRecord.Token] = &inventoryRecord{
		record:   record,
		comments: make(map[string]www.Comment),
	}

	b.loadRecordMetadata(record)

	// update inventory count
	b._updateInventoryCountOfPropStatus(record.Status, nil)

	// update count of user proposals
	err := b._updateCountOfUserProposals(t)
	if err != nil {
		return fmt.Errorf("_newInventoryRecord: _updateCountOfUserProposals %v", err)
	}

	return nil
}

// newInventoryRecord adds a record to the inventory.
//
// This function must be called WITHOUT the mutex held.
func (b *backend) newInventoryRecord(record pd.Record) error {
	b.Lock()
	defer b.Unlock()
	return b._newInventoryRecord(record)
}

// updateInventoryRecord updates an existing record.
//
// This function must be called WITH the mutex held.
func (b *backend) _updateInventoryRecord(record pd.Record) error {
	ir, ok := b.inventory[record.CensorshipRecord.Token]
	if !ok {
		return fmt.Errorf("inventory record not found: %v", record.CensorshipRecord.Token)
	}

	// update inventory count
	b._updateInventoryCountOfPropStatus(record.Status, &ir.record.Status)

	// update record
	ir.record = record
	b.inventory[record.CensorshipRecord.Token] = ir
	b.loadRecordMetadata(record)

	return nil
}

// updateInventoryCount updates the count of proposals by each status
//
// this function must be called WITH the mutex held
func (b *backend) _updateInventoryCountOfPropStatus(status pd.RecordStatusT, oldStatus *pd.RecordStatusT) {
	executeUpdate := func(v int, status www.PropStatusT) {
		switch status {
		case www.PropStatusUnreviewedChanges:
			b.numOfUnvettedChanges += v
		case www.PropStatusNotReviewed:
			b.numOfUnvetted += v
		case www.PropStatusCensored:
			b.numOfCensored += v
		case www.PropStatusPublic:
			b.numOfPublic += v
		case www.PropStatusAbandoned:
			b.numOfAbandoned += v
		default:
			b.numOfInvalid += v
		}
	}
	// decrease count for old status
	if oldStatus != nil {
		executeUpdate(-1, convertPropStatusFromPD(*oldStatus))
	}
	// increase count for new status
	executeUpdate(1, convertPropStatusFromPD(status))
}

// _updateInventoryCountOfUserProposals updates the count of proposals per user ID
// Must be called WITH the mutex held
func (b *backend) _updateCountOfUserProposals(token string) error {
	// get inventory record by token
	ir, ok := b.inventory[token]
	if !ok {
		return fmt.Errorf("inventory record not found: %v", token)
	}

	// get user public key
	pk := ir.proposalMD.PublicKey

	// find user id
	uid, ok := b.userPubkeys[pk]
	if !ok {
		return fmt.Errorf("User not found %v", uid)
	}

	// update count of proposals by user id
	b.numOfPropsByUserID[uid]++

	return nil
}

// loadRecord load an record metadata and comments into inventory.
//
// This function must be called WITH the mutex held.
func (b *backend) loadRecord(record pd.Record) error {
	t := record.CensorshipRecord.Token

	// load record metadata
	b.loadRecordMetadata(record)

	// try to load record comments
	err := b.loadComments(t)
	if err != nil {
		return fmt.Errorf("could not load comments for %s: %v", t, err)
	}

	err = b._loadCommentsLikes(t)
	if err != nil {
		return fmt.Errorf("could not load comment likes for %s: %v", t, err)
	}

	err = b._processCommentsLikesResults(t)
	if err != nil {
		return fmt.Errorf("could not process comment likes results for %s: %v",
			t, err)
	}

	return nil
}

// processCommentsLikesResults calculates the total votes and the result
// votes for each comment of a given proposal. It also calculates the
// resultant action per user per comment
//
// This function must be called WITH the mutext held
func (b *backend) _processCommentsLikesResults(token string) error {
	// get inventory record by token
	ir, ok := b.inventory[token]
	if !ok {
		return fmt.Errorf("inventory record not found: %v", token)
	}

	commentLikes := ir.commentsLikes

	for _, likes := range commentLikes {
		for _, l := range likes {
			_, err := b._updateResultsForCommentLike(l)
			if err != nil {
				return fmt.Errorf(
					"processCommentsLikesResults %v: "+
						"updateResutsForCommentLike %v",
					token, err)
			}
		}
	}

	return nil
}

// loadPropMD decodes backend proposal metadata and stores it inventory object.
//
// This function must be called WITH the mutex held.
func (b *backend) loadPropMD(token, payload string) error {
	f := strings.NewReader(payload)
	d := json.NewDecoder(f)
	var md BackendProposalMetadata
	if err := d.Decode(&md); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}

	b.inventory[token].proposalMD = md
	return nil
}

// loadChanges decodes chnages metadata and stores it inventory object.
//
// This function must be called WITH the mutex held.
func (b *backend) loadChanges(token, payload string) error {
	f := strings.NewReader(payload)
	d := json.NewDecoder(f)
	for {
		var md MDStreamChanges
		if err := d.Decode(&md); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		p := b.inventory[token]
		p.changes = append(p.changes, md)
	}
}

// loadVoteAuthorization decodes vote authorization metadata and stores it
// in the proposal's inventory record.
//
// This function must be called WITH the mutex held.
func (b *backend) loadVoteAuthorization(token, payload string) error {
	f := strings.NewReader(payload)
	d := json.NewDecoder(f)
	var avr decredplugin.AuthorizeVoteReply
	if err := d.Decode(&avr); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	avrWWW := convertAuthorizeVoteReplyFromDecredplugin(avr)
	b.inventory[token].voteAuthorization = avrWWW
	return nil
}

// loadVoting decodes voting metadata and stores it inventory object.
//
// This function must be called WITH the mutex held.
func (b *backend) loadVoting(token, payload string) error {
	f := strings.NewReader(payload)
	d := json.NewDecoder(f)
	var md decredplugin.StartVoteReply
	if err := d.Decode(&md); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	p := b.inventory[token]
	p.voting = convertStartVoteReplyFromDecredplugin(md)
	return nil
}

// loadVoteBits decodes voting metadata and stores it inventory object.
//
// This function must be called WITH the mutex held.
func (b *backend) loadVoteBits(token, payload string) error {
	f := strings.NewReader(payload)
	d := json.NewDecoder(f)
	var md decredplugin.StartVote
	if err := d.Decode(&md); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	log.Tracef("loadVoteBits: %v %v", token, payload)
	p := b.inventory[token]
	p.votebits = convertStartVoteFromDecredplugin(md)
	return nil
}

// loadComments calls out to the decred plugin to obtain all comments.
//
// This function must be called WITH the mutex held.
// XXX this call should be converted to run without the mutext held!
func (b *backend) loadComments(t string) error {
	// Load comments journal
	log.Tracef("loadComments: %v", t)

	challenge, err := util.Random(pd.ChallengeSize)
	if err != nil {
		return err
	}

	payload, err := decredplugin.EncodeGetComments(decredplugin.GetComments{
		Token: t,
	})
	if err != nil {
		return err
	}

	pc := pd.PluginCommand{
		Challenge: hex.EncodeToString(challenge),
		ID:        decredplugin.ID,
		Command:   decredplugin.CmdGetComments,
		CommandID: decredplugin.CmdGetComments,
		Payload:   string(payload),
	}

	responseBody, err := b.makeRequest(http.MethodPost,
		pd.PluginCommandRoute, pc)
	if err != nil {
		return err
	}

	var reply pd.PluginCommandReply
	err = json.Unmarshal(responseBody, &reply)
	if err != nil {
		return fmt.Errorf("Could not unmarshal "+
			"PluginCommandReply: %v", err)
	}

	// Verify the challenge.
	err = util.VerifyChallenge(b.cfg.Identity, challenge, reply.Response)
	if err != nil {
		return err
	}

	// Decode plugin reply
	gcr, err := decredplugin.DecodeGetCommentsReply([]byte(reply.Payload))
	if err != nil {
		return err
	}

	// Fill map
	for _, v := range gcr.Comments {
		c := b._convertDecredCommentToWWWComment(v)
		b.inventory[t].comments[v.CommentID] = c
	}

	log.Tracef("loadComments: %v inserted %v", t, len(gcr.Comments))

	return nil
}

// loadComments calls out to the decred plugin to obtain all comments likes
// of a given proposal
//
// This function must be called WITH the mutex held.
func (b *backend) _loadCommentsLikes(token string) error {
	log.Tracef("loadCommentsLikes %v", token)

	challenge, err := util.Random(pd.ChallengeSize)
	if err != nil {
		return err
	}

	payload, err := decredplugin.EncodeGetProposalCommentsLikes(
		decredplugin.GetProposalCommentsLikes{
			Token: token,
		})
	if err != nil {
		return err
	}

	pc := pd.PluginCommand{
		Challenge: hex.EncodeToString(challenge),
		ID:        decredplugin.ID,
		Command:   decredplugin.CmdProposalCommentsLikes,
		CommandID: decredplugin.CmdProposalCommentsLikes,
		Payload:   string(payload),
	}

	responseBody, err := b.makeRequest(http.MethodPost,
		pd.PluginCommandRoute, pc)
	if err != nil {
		return err
	}

	var reply pd.PluginCommandReply
	err = json.Unmarshal(responseBody, &reply)
	if err != nil {
		return fmt.Errorf("Could not unmarshal "+
			"PluginCommandReply: %v", err)
	}

	// Verify the challenge.
	err = util.VerifyChallenge(b.cfg.Identity, challenge, reply.Response)
	if err != nil {
		return err
	}

	// Decode plugin reply
	gpclr, err := decredplugin.DecodeGetProposalCommentsLikesReply(
		[]byte(reply.Payload))
	if err != nil {
		return err
	}

	b.inventory[token].commentsLikes = make(map[string][]www.LikeComment)
	for _, v := range gpclr.CommentsLikes {
		lc := convertDecredLikeCommentToWWWLikeComment(v)
		cls := b.inventory[token].commentsLikes[lc.CommentID]
		b.inventory[token].commentsLikes[lc.CommentID] = append(cls, lc)
	}

	log.Tracef("loadCommentsLikes: %v inserted %v", token,
		len(gpclr.CommentsLikes))

	return nil
}

// loadRecordMetadata load an entire record metadata into inventory.
//
// This function must be called WITH the mutex held.
func (b *backend) loadRecordMetadata(v pd.Record) {
	t := v.CensorshipRecord.Token

	// Fish metadata out as well
	var err error
	for _, m := range v.Metadata {
		switch m.ID {
		case mdStreamGeneral:
			err = b.loadPropMD(t, m.Payload)
			if err != nil {
				log.Errorf("initializeInventory "+
					"could not load metadata: %v",
					err)
				continue
			}
		case mdStreamChanges:
			err = b.loadChanges(t, m.Payload)
			if err != nil {
				log.Errorf("initializeInventory "+
					"could not load changes: %v",
					err)
				continue
			}
		case decredplugin.MDStreamAuthorizeVote:
			err = b.loadVoteAuthorization(t, m.Payload)
			if err != nil {
				log.Errorf("initializeInventory "+
					"could not load vote authorization: %v", err)
				continue
			}
		case decredplugin.MDStreamVoteBits:
			err = b.loadVoteBits(t, m.Payload)
			if err != nil {
				log.Errorf("initializeInventory "+
					"could not load vote bits: %v", err)
				continue
			}
		case decredplugin.MDStreamVoteSnapshot:
			err = b.loadVoting(t, m.Payload)
			if err != nil {
				log.Errorf("initializeInventory "+
					"could not load vote snapshot: %v", err)
				continue
			}
		default:
			// log error but proceed
			log.Errorf("initializeInventory: invalid "+
				"metadata stream ID %v token %v",
				m.ID, t)
		}
	}
}

// initializeInventory initializes the inventory map and loads it with a
// InventoryReply.
//
// This function must be called WITH the mutex held.
func (b *backend) initializeInventory(inv *pd.InventoryReply) error {
	b.inventory = make(map[string]*inventoryRecord)

	for _, v := range append(inv.Vetted, inv.Branches...) {
		err := b._newInventoryRecord(v)
		if err != nil {
			return err
		}
		err = b.loadRecord(v)
		if err != nil {
			return err
		}
	}

	return nil
}

// _getInventoryRecord reads an inventory record from the inventory cache.
//
// This function must be called WITH the mutex held.
func (b *backend) _getInventoryRecord(token string) (inventoryRecord, error) {
	r, ok := b.inventory[token]
	if !ok {
		return inventoryRecord{}, fmt.Errorf("inventory record not found %v", token)
	}
	return *r, nil
}

// getInventoryRecord returns an inventory record from the inventory cache.
//
// This function must be called WITHOUT the mutex held.
func (b *backend) getInventoryRecord(token string) (inventoryRecord, error) {
	b.RLock()
	defer b.RUnlock()
	return b._getInventoryRecord(token)
}

// getInventoryRecordComment returns a comment from the inventory given its
// record token and the comment id.
//
// This function must be called WITH the mutex held.
func (b *backend) _getInventoryRecordComment(token string, commentID string) (*www.Comment, error) {
	comment, ok := b.inventory[token].comments[commentID]
	if !ok {
		return nil, fmt.Errorf("comment not found %v: %v", token, commentID)
	}
	return &comment, nil
}

// _setRecordComment sets a comment alongside the record's comments (if any)
// this can be used for adding or updating a comment
//
// This function must be called WITH the mutex held
func (b *backend) _setRecordComment(comment www.Comment) error {
	// Sanity check
	_, ok := b.inventory[comment.Token]
	if !ok {
		return fmt.Errorf("inventory record not found: %v", comment.Token)
	}

	// set record comment
	b.inventory[comment.Token].comments[comment.CommentID] = comment

	return nil
}

// setRecordComment sets a comment alongside the record's comments (if any)
// this can be used for adding or updating a comment
//
// This function must be called WITHOUT the mutex held
func (b *backend) setRecordComment(comment www.Comment) error {
	b.Lock()
	defer b.Unlock()
	return b._setRecordComment(comment)
}

// setRecordVoting sets the voting of a proposal
// this can be used for adding or updating a proposal voting
//
// This function must be called WITH the mutex held
func (b *backend) _setRecordVoting(token string, sv www.StartVote, svr www.StartVoteReply) error {
	// Sanity check
	ir, ok := b.inventory[token]
	if !ok {
		return fmt.Errorf("inventory record not found: %v", token)
	}

	// update record
	ir.voting = svr
	ir.votebits = sv
	b.inventory[token] = ir

	return nil
}

// setRecordVoteAuthorization sets the vote authorization metadata for the
// specified inventory record.
//
// This function must be called WITH the mutex held.
func (b *backend) _setRecordVoteAuthorization(token string, avr www.AuthorizeVoteReply) error {
	// Sanity check
	_, ok := b.inventory[token]
	if !ok {
		return fmt.Errorf("inventory record not found %v", token)
	}

	// Set vote authorization
	b.inventory[token].voteAuthorization = avr

	return nil
}

// setRecordVoteAuthorization sets the vote authorization metadata for the
// specified inventory record.
//
// This function must be called WITHOUT the mutex held.
func (b *backend) setRecordVoteAuthorization(token string, avr www.AuthorizeVoteReply) error {
	b.Lock()
	defer b.Unlock()
	return b._setRecordVoteAuthorization(token, avr)
}

// _getProposal returns a single proposal by its token
//
// This function must be called WITH the mutex held.
func (b *backend) _getProposal(token string) (www.ProposalRecord, error) {
	ir, err := b._getInventoryRecord(token)
	if err != nil {
		return www.ProposalRecord{}, err
	}
	pr := b._convertPropFromInventoryRecord(ir)
	return pr, nil
}

// _getAllProposals returns all of the proposals in the inventory.
//
// This function must be called WITH the mutex held.
func (b *backend) _getAllProposals() []www.ProposalRecord {
	allProposals := make([]www.ProposalRecord, 0, len(b.inventory))
	for _, vv := range b.inventory {
		v := b._convertPropFromInventoryRecord(*vv)

		// Set the number of comments.
		v.NumComments = uint(len(vv.comments))

		// Look up and set the user id and username.
		var ok bool
		v.UserId, ok = b.userPubkeys[v.PublicKey]
		if ok {
			v.Username = b.getUsernameById(v.UserId)
		} else {
			log.Infof("%v", spew.Sdump(b.userPubkeys))
			log.Errorf("user not found for public key %v, for proposal %v",
				v.PublicKey, v.CensorshipRecord.Token)
		}

		allProposals = append(allProposals, v)
	}

	return allProposals
}

// getProposals returns a list of proposals that adheres to the requirements
// specified in the provided request.
//
// This function must be called WITHOUT the mutex held.
func (b *backend) getProposals(pr proposalsRequest) []www.ProposalRecord {
	b.RLock()
	allProposals := b._getAllProposals()
	b.RUnlock()
	sort.Slice(allProposals, func(i, j int) bool {
		// sort by older timestamp first, if timestamps are different
		// from each other
		if allProposals[i].Timestamp != allProposals[j].Timestamp {
			return allProposals[i].Timestamp < allProposals[j].Timestamp
		}

		// otherwise sort by token
		return allProposals[i].CensorshipRecord.Token >
			allProposals[j].CensorshipRecord.Token
	})

	// pageStarted stores whether or not it's okay to start adding
	// proposals to the array. If the after or before parameter is
	// supplied, we must find the beginning (or end) of the page first.
	pageStarted := (pr.After == "" && pr.Before == "")
	beforeIdx := -1
	proposals := make([]www.ProposalRecord, 0)

	// Iterate in reverse order because they're sorted by oldest timestamp
	// first.
	for i := len(allProposals) - 1; i >= 0; i-- {
		proposal := allProposals[i]

		// Filter by user if it's provided.
		if pr.UserId != "" && pr.UserId != proposal.UserId {
			continue
		}

		// Filter by the state.
		if val, ok := pr.StateMap[proposal.State]; !ok || !val {
			continue
		}

		if pageStarted {
			proposals = append(proposals, proposal)
			if len(proposals) >= www.ProposalListPageSize {
				break
			}
		} else if pr.After != "" {
			// The beginning of the page has been found, so
			// the next public proposal is added.
			pageStarted = proposal.CensorshipRecord.Token == pr.After
		} else if pr.Before != "" {
			// The end of the page has been found, so we'll
			// have to iterate in the other direction to
			// add the proposals; save the current index.
			if proposal.CensorshipRecord.Token == pr.Before {
				beforeIdx = i
				break
			}
		}
	}

	// If beforeIdx is set, the caller is asking for vetted proposals whose
	// last result is before the provided proposal.
	if beforeIdx >= 0 {
		for _, proposal := range allProposals[beforeIdx+1:] {
			// Filter by user if it's provided.
			if pr.UserId != "" && pr.UserId != proposal.UserId {
				continue
			}

			// Filter by the state.
			if val, ok := pr.StateMap[proposal.State]; !ok || !val {
				continue
			}

			// The iteration direction is oldest -> newest,
			// so proposals are prepended to the array so
			// the result will be newest -> oldest.
			proposals = append([]www.ProposalRecord{proposal},
				proposals...)
			if len(proposals) >= www.ProposalListPageSize {
				break
			}
		}
	}

	return proposals
}
