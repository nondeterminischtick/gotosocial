// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
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
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package status

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/superseriousbusiness/gotosocial/internal/ap"
	apimodel "github.com/superseriousbusiness/gotosocial/internal/api/model"
	"github.com/superseriousbusiness/gotosocial/internal/config"
	"github.com/superseriousbusiness/gotosocial/internal/db"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/id"
	"github.com/superseriousbusiness/gotosocial/internal/log"
	"github.com/superseriousbusiness/gotosocial/internal/messages"
	"github.com/superseriousbusiness/gotosocial/internal/text"
	"github.com/superseriousbusiness/gotosocial/internal/typeutils"
	"github.com/superseriousbusiness/gotosocial/internal/uris"
	"github.com/superseriousbusiness/gotosocial/internal/util"
)

// Create processes the given form to create a new status, returning the api model representation of that status if it's OK.
//
// Precondition: the form's fields should have already been validated and normalized by the caller.
func (p *Processor) Create(
	ctx context.Context,
	requester *gtsmodel.Account,
	application *gtsmodel.Application,
	form *apimodel.StatusCreateRequest,
) (
	*apimodel.Status,
	gtserror.WithCode,
) {
	// Ensure account populated; we'll need settings.
	if err := p.state.DB.PopulateAccount(ctx, requester); err != nil {
		log.Errorf(ctx, "error(s) populating account, will continue: %s", err)
	}

	// Generate new ID for status.
	statusID := id.NewULID()

	// Generate necessary URIs for username, to build status URIs.
	accountURIs := uris.GenerateURIsForAccount(requester.Username)

	// Get current time.
	now := time.Now()

	//override the status URL if provided
	//if we want to post a blog to fedi then we can post a status
	//but set the source URL to point to the blogpost itself
	statusURL := accountURIs.StatusesURI + "/" + statusID
	if form.URL != "" {
		statusURL = form.URL
	}

	//then the javascript on the blog post can search GTS for its URL
	//and pull in any replies to render below its content

	isBlogToFederate := false
	blogPath := config.GetProtocol() + "://" + config.GetHost() + "/blog"
	articlePath := config.GetProtocol() + "://" + config.GetHost() + "/writing"

	status := &gtsmodel.Status{
		ID:                       statusID,
		URI:                      accountURIs.StatusesURI + "/" + statusID,
		URL:                      statusURL,
		CreatedAt:                now,
		UpdatedAt:                now,
		Local:                    util.Ptr(true),
		Account:                  requester,
		AccountID:                requester.ID,
		AccountURI:               requester.URI,
		ActivityStreamsType:      ap.ObjectNote,
		Sensitive:                &form.Sensitive,
		CreatedWithApplicationID: application.ID,
		Text:                     form.Status,
	}

	if form.Poll != nil {
		// Update the status AS type to "Question".
		status.ActivityStreamsType = ap.ActivityQuestion

		// Create new poll for status from form.
		secs := time.Duration(form.Poll.ExpiresIn)
		status.Poll = &gtsmodel.Poll{
			ID:         id.NewULID(),
			Multiple:   &form.Poll.Multiple,
			HideCounts: &form.Poll.HideTotals,
			Options:    form.Poll.Options,
			StatusID:   statusID,
			Status:     status,
			ExpiresAt:  now.Add(secs * time.Second),
		}

		// Set poll ID on the status.
		status.PollID = status.Poll.ID
	}

	// Check + attach in-reply-to status.
	if errWithCode := p.processInReplyTo(ctx,
		requester,
		status,
		form.InReplyToID,
	); errWithCode != nil {
		return nil, errWithCode
	}

	if form.InReplyToID == "" {
		//if a post not a reply and just contains a blog url and nothing else, then assume I want to federate it
		words := strings.Split(status.Text, " ")

		if len(words) == 1 {
			url := words[0]
			isBlogToFederate = strings.HasPrefix(url, blogPath) || strings.HasPrefix(url, articlePath)

			//add trailing slash if missing
			if url[len(url)-1] != '/' {
				url = url + "/"
			}

			if isBlogToFederate {
				//before doing this madness, make sure we've not federated this blog before
				status, err := p.state.DB.GetStatusByURL(ctx, url)

				if err != nil && err.Error() == "no entries" {
					//try and get meta tag from url and rewrite status text and url
					//if no meta tag found, just post the status normally
					newText := ""
					newText, err = getNewTextFromBlogMeta(url, "fedi")

					if err == nil && newText != "" {
						form.Status = newText + "\n\n" + form.Status
						status.Text = form.Status
						status.URL = url
					}

				} else if status != nil {
					//Already federated this blog post so do nothing, the post will be dealt with as normal
				}
			}
		} else {
			// not a single word status carry on as normal
		}
	}

	if errWithCode := p.processThreadID(ctx, status); errWithCode != nil {
		return nil, errWithCode
	}

	if errWithCode := p.processMediaIDs(ctx, form, requester.ID, status); errWithCode != nil {
		return nil, errWithCode
	}

	if err := p.processVisibility(ctx, form, requester.Settings.Privacy, status); err != nil {
		return nil, gtserror.NewErrorInternalError(err)
	}

	// Process policy AFTER visibility as it relies
	// on status.Visibility and form.Visibility being set.
	if errWithCode := processInteractionPolicy(form, requester.Settings, status); errWithCode != nil {
		return nil, errWithCode
	}

	if err := processLanguage(form, requester.Settings.Language, status); err != nil {
		return nil, gtserror.NewErrorInternalError(err)
	}

	if err := p.processContent(ctx, p.parseMention, form, status); err != nil {
		return nil, gtserror.NewErrorInternalError(err)
	}

	if status.Poll != nil {
		// Try to insert the new status poll in the database.
		if err := p.state.DB.PutPoll(ctx, status.Poll); err != nil {
			err := gtserror.Newf("error inserting poll in db: %w", err)
			return nil, gtserror.NewErrorInternalError(err)
		}
	}

	if !isBlogToFederate {
		// this new status wasn't a manual blog publish
		// in which case it's a normal post, that we now want to publish to Hugo via GitHub
		publishedUrl, err := publishNote(status)
		if err != nil {
			return nil, gtserror.NewErrorInternalError(err)
		}

		status.URL = publishedUrl
	}

	// Insert this new status in the database.
	if err := p.state.DB.PutStatus(ctx, status); err != nil {
		return nil, gtserror.NewErrorInternalError(err)
	}

	// send it back to the client API worker for async side-effects.
	p.state.Workers.Client.Queue.Push(&messages.FromClientAPI{
		APObjectType:   ap.ObjectNote,
		APActivityType: ap.ActivityCreate,
		GTSModel:       status,
		Origin:         requester,
	})

	if status.Poll != nil {
		// Now that the status is inserted, and side effects queued,
		// attempt to schedule an expiry handler for the status poll.
		if err := p.polls.ScheduleExpiry(ctx, status.Poll); err != nil {
			log.Errorf(ctx, "error scheduling poll expiry: %v", err)
		}
	}

	// If the new status replies to a status that
	// replies to us, use our reply as an implicit
	// accept of any pending interaction.
	implicitlyAccepted, errWithCode := p.implicitlyAccept(ctx,
		requester, status,
	)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// If we ended up implicitly accepting, mark the
	// replied-to status as no longer pending approval
	// so it's serialized properly via the API.
	if implicitlyAccepted {
		status.InReplyTo.PendingApproval = util.Ptr(false)
	}

	return p.c.GetAPIStatus(ctx, requester, status)
}

func getNewTextFromBlogMeta(url string, name string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		resp = nil
	}

	defer resp.Body.Close()

	tkn := html.NewTokenizer(resp.Body)
	var tokenFound bool

	for {
		tt := tkn.Next()

		switch {
		case tt == html.ErrorToken:
			//error or end of document without finding meta tag
			return "", errors.New("Not found")

		case tt == html.StartTagToken:
			t := tkn.Token()

			if t.Data == `body` {
				//reached body without finding the meta tag
				return "", errors.New("Not found")
			}

			if t.Data == "meta" {
				for _, attr := range t.Attr {
					if attr.Key == "name" && attr.Val == name {
						tokenFound = true
					}

					if tokenFound && attr.Key == "content" {
						return attr.Val, nil
					}
				}
			}
		}
	}
}

func (p *Processor) processInReplyTo(ctx context.Context, requester *gtsmodel.Account, status *gtsmodel.Status, inReplyToID string) gtserror.WithCode {
	if inReplyToID == "" {
		// Not a reply.
		// Nothing to do.
		return nil
	}

	// Fetch target in-reply-to status (checking visibility).
	inReplyTo, errWithCode := p.c.GetVisibleTargetStatus(ctx,
		requester,
		inReplyToID,
		nil,
	)
	if errWithCode != nil {
		return errWithCode
	}

	// If this is a boost, unwrap it to get source status.
	inReplyTo, errWithCode = p.c.UnwrapIfBoost(ctx,
		requester,
		inReplyTo,
	)
	if errWithCode != nil {
		return errWithCode
	}

	// Ensure valid reply target for requester.
	policyResult, err := p.intFilter.StatusReplyable(ctx,
		requester,
		inReplyTo,
	)
	if err != nil {
		err := gtserror.Newf("error seeing if status %s is replyable: %w", status.ID, err)
		return gtserror.NewErrorInternalError(err)
	}

	if policyResult.Forbidden() {
		const errText = "you do not have permission to reply to this status"
		err := gtserror.New(errText)
		return gtserror.NewErrorForbidden(err, errText)
	}

	// Derive pendingApproval status.
	var pendingApproval bool
	switch {
	case policyResult.WithApproval():
		// We're allowed to do
		// this pending approval.
		pendingApproval = true

	case policyResult.MatchedOnCollection():
		// We're permitted to do this, but since
		// we matched due to presence in a followers
		// or following collection, we should mark
		// as pending approval and wait until we can
		// prove it's been Accepted by the target.
		pendingApproval = true

		if *inReplyTo.Local {
			// If the target is local we don't need
			// to wait for an Accept from remote,
			// we can just preapprove it and have
			// the processor create the Accept.
			status.PreApproved = true
		}

	case policyResult.Permitted():
		// We're permitted to do this
		// based on another kind of match.
		pendingApproval = false
	}

	status.PendingApproval = &pendingApproval

	// Set status fields from inReplyTo.
	status.InReplyToID = inReplyTo.ID
	status.InReplyTo = inReplyTo
	status.InReplyToURI = inReplyTo.URI
	status.InReplyToAccountID = inReplyTo.AccountID

	return nil
}

func (p *Processor) processThreadID(ctx context.Context, status *gtsmodel.Status) gtserror.WithCode {
	// Status takes the thread ID of
	// whatever it replies to, if set.
	//
	// Might not be set if status is local
	// and replies to a remote status that
	// doesn't have a thread ID yet.
	//
	// If so, we can just thread from this
	// status onwards instead, since this
	// is where the relevant part of the
	// thread starts, from the perspective
	// of our instance at least.
	if status.InReplyTo != nil &&
		status.InReplyTo.ThreadID != "" {
		// Just inherit threadID from parent.
		status.ThreadID = status.InReplyTo.ThreadID
		return nil
	}

	// Mark new thread (or threaded
	// subsection) starting from here.
	threadID := id.NewULID()
	if err := p.state.DB.PutThread(
		ctx,
		&gtsmodel.Thread{
			ID: threadID,
		},
	); err != nil {
		err := gtserror.Newf("error inserting new thread in db: %w", err)
		return gtserror.NewErrorInternalError(err)
	}

	// Future replies to this status
	// (if any) will inherit this thread ID.
	status.ThreadID = threadID

	return nil
}

func (p *Processor) processMediaIDs(ctx context.Context, form *apimodel.StatusCreateRequest, thisAccountID string, status *gtsmodel.Status) gtserror.WithCode {
	if form.MediaIDs == nil {
		return nil
	}

	// Get minimum allowed char descriptions.
	minChars := config.GetMediaDescriptionMinChars()

	attachments := []*gtsmodel.MediaAttachment{}
	attachmentIDs := []string{}

	for _, mediaID := range form.MediaIDs {
		attachment, err := p.state.DB.GetAttachmentByID(ctx, mediaID)
		if err != nil && !errors.Is(err, db.ErrNoEntries) {
			err := gtserror.Newf("error fetching media from db: %w", err)
			return gtserror.NewErrorInternalError(err)
		}

		if attachment == nil {
			text := fmt.Sprintf("media %s not found", mediaID)
			return gtserror.NewErrorBadRequest(errors.New(text), text)
		}

		if attachment.AccountID != thisAccountID {
			text := fmt.Sprintf("media %s does not belong to account", mediaID)
			return gtserror.NewErrorBadRequest(errors.New(text), text)
		}

		if attachment.StatusID != "" || attachment.ScheduledStatusID != "" {
			text := fmt.Sprintf("media %s already attached to status", mediaID)
			return gtserror.NewErrorBadRequest(errors.New(text), text)
		}

		if length := len([]rune(attachment.Description)); length < minChars {
			text := fmt.Sprintf("media %s description too short, at least %d required", mediaID, minChars)
			return gtserror.NewErrorBadRequest(errors.New(text), text)
		}

		attachments = append(attachments, attachment)
		attachmentIDs = append(attachmentIDs, attachment.ID)
	}

	status.Attachments = attachments
	status.AttachmentIDs = attachmentIDs
	return nil
}

func (p *Processor) processVisibility(
	ctx context.Context,
	form *apimodel.StatusCreateRequest,
	accountDefaultVis gtsmodel.Visibility,
	status *gtsmodel.Status,
) error {
	switch {
	// Visibility set on form, use that.
	case form.Visibility != "":
		status.Visibility = typeutils.APIVisToVis(form.Visibility)

	// Fall back to account default, set
	// this back on the form for later use.
	case accountDefaultVis != "":
		status.Visibility = accountDefaultVis
		form.Visibility = p.converter.VisToAPIVis(ctx, accountDefaultVis)

	// What? Fall back to global default, set
	// this back on the form for later use.
	default:
		status.Visibility = gtsmodel.VisibilityDefault
		form.Visibility = p.converter.VisToAPIVis(ctx, gtsmodel.VisibilityDefault)
	}

	// Set federated according to "local_only" field,
	// assuming federated (ie., not local-only) by default.
	localOnly := util.PtrOrValue(form.LocalOnly, false)
	status.Federated = util.Ptr(!localOnly)

	return nil
}

func processInteractionPolicy(
	form *apimodel.StatusCreateRequest,
	settings *gtsmodel.AccountSettings,
	status *gtsmodel.Status,
) gtserror.WithCode {

	// If policy is set on the
	// form then prefer this.
	//
	// TODO: prevent scope widening by
	// limiting interaction policy if
	// inReplyTo status has a stricter
	// interaction policy than this one.
	if form.InteractionPolicy != nil {
		p, err := typeutils.APIInteractionPolicyToInteractionPolicy(
			form.InteractionPolicy,
			form.Visibility,
		)

		if err != nil {
			errWithCode := gtserror.NewErrorBadRequest(err, err.Error())
			return errWithCode
		}

		status.InteractionPolicy = p
		return nil
	}

	switch status.Visibility {

	case gtsmodel.VisibilityPublic:
		// Take account's default "public" policy if set.
		if p := settings.InteractionPolicyPublic; p != nil {
			status.InteractionPolicy = p
		}

	case gtsmodel.VisibilityUnlocked:
		// Take account's default "unlisted" policy if set.
		if p := settings.InteractionPolicyUnlocked; p != nil {
			status.InteractionPolicy = p
		}

	case gtsmodel.VisibilityFollowersOnly,
		gtsmodel.VisibilityMutualsOnly:
		// Take account's default followers-only policy if set.
		// TODO: separate policy for mutuals-only vis.
		if p := settings.InteractionPolicyFollowersOnly; p != nil {
			status.InteractionPolicy = p
		}

	case gtsmodel.VisibilityDirect:
		// Take account's default direct policy if set.
		if p := settings.InteractionPolicyDirect; p != nil {
			status.InteractionPolicy = p
		}
	}

	// If no policy set by now, status interaction
	// policy will be stored as nil, which just means
	// "fall back to global default policy". We avoid
	// setting it explicitly to save space.
	return nil
}

func processLanguage(form *apimodel.StatusCreateRequest, accountDefaultLanguage string, status *gtsmodel.Status) error {
	if form.Language != "" {
		status.Language = form.Language
	} else {
		status.Language = accountDefaultLanguage
	}
	if status.Language == "" {
		return errors.New("no language given either in status create form or account default")
	}
	return nil
}

func (p *Processor) processContent(ctx context.Context, parseMention gtsmodel.ParseMentionFunc, form *apimodel.StatusCreateRequest, status *gtsmodel.Status) error {
	if form.ContentType == "" {
		// If content type wasn't specified, use the author's preferred content-type.
		contentType := apimodel.StatusContentType(status.Account.Settings.StatusContentType)
		form.ContentType = contentType
	}

	// format is the currently set text formatting
	// function, according to the provided content-type.
	var format text.FormatFunc

	// formatInput is a shorthand function to format the given input string with the
	// currently set 'formatFunc', passing in all required args and returning result.
	formatInput := func(formatFunc text.FormatFunc, input string) *text.FormatResult {
		return formatFunc(ctx, parseMention, status.AccountID, status.ID, input)
	}

	switch form.ContentType {
	// None given / set,
	// use default (plain).
	case "":
		fallthrough

	// Format status according to text/plain.
	case apimodel.StatusContentTypePlain:
		format = p.formatter.FromPlain

	// Format status according to text/markdown.
	case apimodel.StatusContentTypeMarkdown:
		format = p.formatter.FromMarkdown

	// Unknown.
	default:
		return fmt.Errorf("invalid status format: %q", form.ContentType)
	}

	// Sanitize status text and format.
	contentRes := formatInput(format, form.Status)

	// Collect formatted results.
	status.Content = contentRes.HTML
	status.Mentions = append(status.Mentions, contentRes.Mentions...)
	status.Emojis = append(status.Emojis, contentRes.Emojis...)
	status.Tags = append(status.Tags, contentRes.Tags...)

	// From here-on-out just use emoji-only
	// plain-text formatting as the FormatFunc.
	format = p.formatter.FromPlainEmojiOnly

	// Sanitize content warning and format.
	spoiler := text.SanitizeToPlaintext(form.SpoilerText)
	warningRes := formatInput(format, spoiler)

	// Collect formatted results.
	status.ContentWarning = warningRes.HTML
	status.Emojis = append(status.Emojis, warningRes.Emojis...)

	if status.Poll != nil {
		for i := range status.Poll.Options {
			// Sanitize each option title name and format.
			option := text.SanitizeToPlaintext(status.Poll.Options[i])
			optionRes := formatInput(format, option)

			// Collect each formatted result.
			status.Poll.Options[i] = optionRes.HTML
			status.Emojis = append(status.Emojis, optionRes.Emojis...)
		}
	}

	// Gather all the database IDs from each of the gathered status mentions, tags, and emojis.
	status.MentionIDs = util.Gather(nil, status.Mentions, func(mention *gtsmodel.Mention) string { return mention.ID })
	status.TagIDs = util.Gather(nil, status.Tags, func(tag *gtsmodel.Tag) string { return tag.ID })
	status.EmojiIDs = util.Gather(nil, status.Emojis, func(emoji *gtsmodel.Emoji) string { return emoji.ID })

	if status.ContentWarning != "" && len(status.AttachmentIDs) > 0 {
		// If a content-warning is set, and
		// the status contains media, always
		// set the status sensitive flag.
		status.Sensitive = util.Ptr(true)
	}

	return nil
}
