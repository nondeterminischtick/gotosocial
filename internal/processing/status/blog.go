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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	//"mime/multipart"

	"github.com/superseriousbusiness/gotosocial/internal/config"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/util"
)

// payload to send to publish new status to github
type NoteMentionPayload struct {
	Username string `json:"username"`
	Url      string `json:"url"`
	Acct     string `json:"acct"`
}

type NotePayload struct {
	ID            string               `json:"id"`
	Published     time.Time            `json:"published"`
	Title         string               `json:"title"`
	ReplyTo       string               `json:"replyTo"`
	ReplyToName   string               `json:"replyToName"`
	ReplyToAuthor string               `json:"replyToAuthor"`
	Content       string               `json:"content"`
	Mentions      []NoteMentionPayload `json:"mentions"`
}

type BookmarkPayload struct {
	Bookmarked time.Time `json:"bookmarked"`
	ID         string    `json:"id"`
	Url        string    `json:"url"`
	Username   string    `json:"username"`
	Domain     string    `json:"domain"`
	AcctUrl    string    `json:"accturl"`
}

func publishNote(newStatus *gtsmodel.Status) (string, error) {
	//publish the post to hugo blog via GitPubHub endpoint
	notesPath := config.GetProtocol() + "://" + config.GetHost() + "/notes"

	data := NotePayload{
		ID:            newStatus.ID,
		Published:     newStatus.CreatedAt,
		Title:         newStatus.ContentWarning,
		ReplyTo:       "",
		ReplyToName:   "",
		ReplyToAuthor: "",
		Content:       newStatus.Text,
	}

	// Convert GTS mentions model to simple model for payload
	jsonMentions := make([]NoteMentionPayload, 0, len(newStatus.Mentions))

	for _, mention := range newStatus.Mentions {
		if !mention.TargetAccount.IsLocal() {
			// Domain may be in Punycode,
			domain, err := util.DePunify(mention.TargetAccount.Domain)
			if err != nil {
				continue
			}

			jsonMention := NoteMentionPayload{
				Username: "@" + mention.TargetAccount.Username,
				Url:      mention.TargetAccount.URL,
				Acct:     "@" + mention.TargetAccount.Username + "@" + domain,
			}

			jsonMentions = append(jsonMentions, jsonMention)

		}
	}

	data.Mentions = jsonMentions

	if newStatus.InReplyToID != "" {
		// reply to fedi post so fill in reply info
		data.ReplyTo = newStatus.InReplyTo.URL
		data.ReplyToAuthor = newStatus.InReplyTo.Account.Username

	} else {
		// not a fedi reply but we look for a post pattern to see if it's specifically citing a url
		// if at least 3 lines long and the 2nd line is a valid url then we assume a post
		// like the e.g. below (Title \n Url \n Content)

		/// How The West Was Won
		/// https://example.com/opinions/article/29837489897/
		/// I really enjoyed reading this...

		lines := strings.Split(newStatus.Text, "\n")
		newContent := ""

		if len(lines) > 2 {
			url := lines[1]
			if isValidUrl(url) {
				// add the first 2 lines to the meta data
				data.ReplyTo = url
				data.ReplyToName = lines[0]

				// remove the first 2 lines from the content
				for i, line := range lines {
					if i > 1 {
						newContent = newContent + line
					}
				}

				data.Content = newContent
			}
		}
	}

	// loop through newStatus.Attachments and copy them to the static blog storage
	if len(newStatus.Attachments) > 0 {
		// ideally you'd push these files to git with the status
		// but sending images to github just to rsync them back to the same server seems mad
		basePath := config.GetStorageLocalBasePath()

		for _, attachment := range newStatus.Attachments {
			attachmentPath := strings.Split(attachment.File.Path, "/")
			attachmentFileName := attachmentPath[len(attachmentPath)-1]
			caption := attachment.Description

			sourcePath := basePath + "/" + attachment.File.Path
			destinationPath := "/var/www/art/notes/" + attachmentFileName
			_, err := copy(sourcePath, destinationPath)
			if err != nil {
				return "", err

			}

			data.Content = data.Content + "\n" + "![" + caption + "](/art/notes/" + attachmentFileName + ")"
		}
	}

	payloadBytes, err := json.Marshal(data)
	if err != nil {
		return "", err
	}

	body := bytes.NewReader(payloadBytes)

	req, err := http.NewRequest("POST", "http://localhost:8085/notes", body)
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Return the status url to the same as the note published to git  yyyy/MMddhhmmdd
	publishedUrl := notesPath + "/" + newStatus.CreatedAt.Format("2006") + "/" + newStatus.CreatedAt.Format("0102150405") + "/"
	return publishedUrl, nil

}

func unpublishNote(createdAt string) error {
	relativePath := createdAt
	// 2023-07-03T14:37:00.940211997+01:00
	relativePath = strings.Replace(relativePath, "-", "/", 1)
	relativePath = strings.Replace(relativePath, "-", "", 1)
	relativePath = strings.Replace(relativePath, "T", "", 1)
	relativePath = strings.Replace(relativePath, ":", "", -1)
	relativePath = strings.Split(relativePath, ".")[0]
	// 2023/0730092025

	req, err := http.NewRequest("DELETE", "http://localhost:8085/notes/"+relativePath, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

func publishBookmark(Id string, targetStatus *gtsmodel.Status, targetAccount *gtsmodel.Account) error {
	// Domain may be in Punycode,
	domain, errDomain := util.DePunify(targetAccount.Domain)
	if errDomain != nil {
		return errDomain
	}

	data := BookmarkPayload{
		Bookmarked: time.Now(),
		ID:         Id,
		Url:        targetStatus.URL,
		Username:   "@" + targetAccount.Username,
		Domain:     domain,
		AcctUrl:    targetAccount.URL,
	}

	payloadBytes, err := json.Marshal(data)
	if err != nil {
		return err
	}

	body := bytes.NewReader(payloadBytes)

	req, err := http.NewRequest("POST", "http://localhost:8085/bookmarks", body)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func unpublishBookmark(id string) error {
	req, err := http.NewRequest("DELETE", "http://localhost:8085/bookmarks/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

func copy(src string, dst string) (int64, error) {
	sourceFileStat, err := os.Stat(src)
	if err != nil {
		return 0, err
	}

	if !sourceFileStat.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", src)
	}

	source, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer source.Close()

	destination, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer destination.Close()
	nBytes, err := io.Copy(destination, source)
	return nBytes, err
}

func isValidUrl(test string) bool {
	_, err := url.ParseRequestURI(test)
	if err != nil {
		return false
	}

	u, err := url.Parse(test)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}

	return true
}
