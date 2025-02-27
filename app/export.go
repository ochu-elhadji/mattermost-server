// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package app

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-server/v6/app/imports"
	"github.com/mattermost/mattermost-server/v6/app/request"
	"github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/mattermost-server/v6/shared/mlog"
	"github.com/mattermost/mattermost-server/v6/store"
)

// We use this map to identify the exportable preferences.
// Here we link the preference category and name, to the name of the relevant field in the import struct.
var exportablePreferences = map[imports.ComparablePreference]string{{
	Category: model.PreferenceCategoryTheme,
	Name:     "",
}: "Theme", {
	Category: model.PreferenceCategoryAdvancedSettings,
	Name:     "feature_enabled_markdown_preview",
}: "UseMarkdownPreview", {
	Category: model.PreferenceCategoryAdvancedSettings,
	Name:     "formatting",
}: "UseFormatting", {
	Category: model.PreferenceCategorySidebarSettings,
	Name:     "show_unread_section",
}: "ShowUnreadSection", {
	Category: model.PreferenceCategoryDisplaySettings,
	Name:     model.PreferenceNameUseMilitaryTime,
}: "UseMilitaryTime", {
	Category: model.PreferenceCategoryDisplaySettings,
	Name:     model.PreferenceNameCollapseSetting,
}: "CollapsePreviews", {
	Category: model.PreferenceCategoryDisplaySettings,
	Name:     model.PreferenceNameMessageDisplay,
}: "MessageDisplay", {
	Category: model.PreferenceCategoryDisplaySettings,
	Name:     "channel_display_mode",
}: "CollapseConsecutive", {
	Category: model.PreferenceCategoryDisplaySettings,
	Name:     "collapse_consecutive_messages",
}: "ColorizeUsernames", {
	Category: model.PreferenceCategoryDisplaySettings,
	Name:     "colorize_usernames",
}: "ChannelDisplayMode", {
	Category: model.PreferenceCategoryTutorialSteps,
	Name:     "",
}: "TutorialStep", {
	Category: model.PreferenceCategoryNotifications,
	Name:     model.PreferenceNameEmailInterval,
}: "EmailInterval",
}

func (a *App) BulkExport(ctx request.CTX, writer io.Writer, outPath string, opts model.BulkExportOpts) *model.AppError {
	var zipWr *zip.Writer
	if opts.CreateArchive {
		var err error
		zipWr = zip.NewWriter(writer)
		defer zipWr.Close()
		writer, err = zipWr.Create("import.jsonl")
		if err != nil {
			return model.NewAppError("BulkExport", "app.export.zip_create.error",
				nil, "err="+err.Error(), http.StatusInternalServerError)
		}
	}

	ctx.Logger().Info("Bulk export: exporting version")
	if err := a.exportVersion(writer); err != nil {
		return err
	}

	ctx.Logger().Info("Bulk export: exporting teams")
	teamNames, err := a.exportAllTeams(writer)
	if err != nil {
		return err
	}

	ctx.Logger().Info("Bulk export: exporting channels")
	if err = a.exportAllChannels(writer, teamNames); err != nil {
		return err
	}

	ctx.Logger().Info("Bulk export: exporting users")
	if err = a.exportAllUsers(writer); err != nil {
		return err
	}

	ctx.Logger().Info("Bulk export: exporting posts")
	attachments, err := a.exportAllPosts(ctx, writer, opts.IncludeAttachments)
	if err != nil {
		return err
	}

	ctx.Logger().Info("Bulk export: exporting emoji")
	emojiPaths, err := a.exportCustomEmoji(writer, outPath, "exported_emoji", !opts.CreateArchive)
	if err != nil {
		return err
	}

	ctx.Logger().Info("Bulk export: exporting direct channels")
	if err = a.exportAllDirectChannels(writer); err != nil {
		return err
	}

	ctx.Logger().Info("Bulk export: exporting direct posts")
	directAttachments, err := a.exportAllDirectPosts(ctx, writer, opts.IncludeAttachments)
	if err != nil {
		return err
	}

	if opts.IncludeAttachments {
		ctx.Logger().Info("Bulk export: exporting file attachments")
		for _, attachment := range attachments {
			if err := a.exportFile(outPath, *attachment.Path, zipWr); err != nil {
				return err
			}
		}
		for _, attachment := range directAttachments {
			if err := a.exportFile(outPath, *attachment.Path, zipWr); err != nil {
				return err
			}
		}
		for _, emojiPath := range emojiPaths {
			if err := a.exportFile(outPath, emojiPath, zipWr); err != nil {
				return err
			}
		}
	}

	return nil
}

func (a *App) exportWriteLine(w io.Writer, line *imports.LineImportData) *model.AppError {
	b, err := json.Marshal(line)
	if err != nil {
		return model.NewAppError("BulkExport", "app.export.export_write_line.json_marshall.error", nil, "", http.StatusBadRequest).Wrap(err)
	}

	if _, err := w.Write(append(b, '\n')); err != nil {
		return model.NewAppError("BulkExport", "app.export.export_write_line.io_writer.error", nil, "", http.StatusBadRequest).Wrap(err)
	}

	return nil
}

func (a *App) exportVersion(writer io.Writer) *model.AppError {
	version := 1
	versionLine := &imports.LineImportData{
		Type:    "version",
		Version: &version,
	}

	return a.exportWriteLine(writer, versionLine)
}

func (a *App) exportAllTeams(writer io.Writer) (map[string]bool, *model.AppError) {
	afterId := strings.Repeat("0", 26)
	teamNames := make(map[string]bool)
	for {
		teams, err := a.Srv().Store().Team().GetAllForExportAfter(1000, afterId)
		if err != nil {
			return nil, model.NewAppError("exportAllTeams", "app.team.get_all.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		}

		if len(teams) == 0 {
			break
		}

		for _, team := range teams {
			afterId = team.Id

			// Skip deleted.
			if team.DeleteAt != 0 {
				continue
			}
			teamNames[team.Name] = true

			teamLine := ImportLineFromTeam(team)
			if err := a.exportWriteLine(writer, teamLine); err != nil {
				return nil, err
			}
		}
	}

	return teamNames, nil
}

func (a *App) exportAllChannels(writer io.Writer, teamNames map[string]bool) *model.AppError {
	afterId := strings.Repeat("0", 26)
	for {
		channels, err := a.Srv().Store().Channel().GetAllChannelsForExportAfter(1000, afterId)

		if err != nil {
			return model.NewAppError("exportAllChannels", "app.channel.get_all.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		}

		if len(channels) == 0 {
			break
		}

		for _, channel := range channels {
			afterId = channel.Id

			// Skip deleted.
			if channel.DeleteAt != 0 {
				continue
			}
			// Skip channels on deleted teams.
			if ok := teamNames[channel.TeamName]; !ok {
				continue
			}

			channelLine := ImportLineFromChannel(channel)
			if err := a.exportWriteLine(writer, channelLine); err != nil {
				return err
			}
		}
	}

	return nil
}

func (a *App) exportAllUsers(writer io.Writer) *model.AppError {
	afterId := strings.Repeat("0", 26)
	for {
		users, err := a.Srv().Store().User().GetAllAfter(1000, afterId)

		if err != nil {
			return model.NewAppError("exportAllUsers", "app.user.get.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		}

		if len(users) == 0 {
			break
		}

		for _, user := range users {
			afterId = user.Id

			// Gathering here the exportable preferences to pass them on to ImportLineFromUser
			exportedPrefs := make(map[string]*string)
			allPrefs, err := a.GetPreferencesForUser(user.Id)
			if err != nil {
				return err
			}
			for _, pref := range allPrefs {
				// We need to manage the special cases
				// Here we manage Tutorial steps
				if pref.Category == model.PreferenceCategoryTutorialSteps {
					pref.Name = ""
					// Then the email interval
				} else if pref.Category == model.PreferenceCategoryNotifications && pref.Name == model.PreferenceNameEmailInterval {
					switch pref.Value {
					case model.PreferenceEmailIntervalNoBatchingSeconds:
						pref.Value = model.PreferenceEmailIntervalImmediately
					case model.PreferenceEmailIntervalFifteenAsSeconds:
						pref.Value = model.PreferenceEmailIntervalFifteen
					case model.PreferenceEmailIntervalHourAsSeconds:
						pref.Value = model.PreferenceEmailIntervalHour
					case "0":
						pref.Value = ""
					}
				}
				id, ok := exportablePreferences[imports.ComparablePreference{
					Category: pref.Category,
					Name:     pref.Name,
				}]
				if ok {
					prefPtr := pref.Value
					if prefPtr != "" {
						exportedPrefs[id] = &prefPtr
					} else {
						exportedPrefs[id] = nil
					}
				}
			}

			userLine := ImportLineFromUser(user, exportedPrefs)

			userLine.User.NotifyProps = a.buildUserNotifyProps(user.NotifyProps)

			// Do the Team Memberships.
			members, err := a.buildUserTeamAndChannelMemberships(user.Id)
			if err != nil {
				return err
			}

			userLine.User.Teams = members

			if err := a.exportWriteLine(writer, userLine); err != nil {
				return err
			}
		}
	}

	return nil
}

func (a *App) buildUserTeamAndChannelMemberships(userID string) (*[]imports.UserTeamImportData, *model.AppError) {
	var memberships []imports.UserTeamImportData

	members, err := a.Srv().Store().Team().GetTeamMembersForExport(userID)

	if err != nil {
		return nil, model.NewAppError("buildUserTeamAndChannelMemberships", "app.team.get_members.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}

	for _, member := range members {
		// Skip deleted.
		if member.DeleteAt != 0 {
			continue
		}

		memberData := ImportUserTeamDataFromTeamMember(member)

		// Do the Channel Memberships.
		channelMembers, err := a.buildUserChannelMemberships(userID, member.TeamId)
		if err != nil {
			return nil, err
		}

		// Get the user theme
		themePreference, nErr := a.Srv().Store().Preference().Get(member.UserId, model.PreferenceCategoryTheme, member.TeamId)
		if nErr == nil {
			memberData.Theme = &themePreference.Value
		}

		memberData.Channels = channelMembers

		memberships = append(memberships, *memberData)
	}

	return &memberships, nil
}

func (a *App) buildUserChannelMemberships(userID string, teamID string) (*[]imports.UserChannelImportData, *model.AppError) {
	var memberships []imports.UserChannelImportData

	members, nErr := a.Srv().Store().Channel().GetChannelMembersForExport(userID, teamID)
	if nErr != nil {
		return nil, model.NewAppError("buildUserChannelMemberships", "app.channel.get_members.app_error", nil, "", http.StatusInternalServerError).Wrap(nErr)
	}

	category := model.PreferenceCategoryFavoriteChannel
	preferences, err := a.GetPreferenceByCategoryForUser(userID, category)
	if err != nil && err.StatusCode != http.StatusNotFound {
		return nil, err
	}

	for _, member := range members {
		memberships = append(memberships, *ImportUserChannelDataFromChannelMemberAndPreferences(member, &preferences))
	}
	return &memberships, nil
}

func (a *App) buildUserNotifyProps(notifyProps model.StringMap) *imports.UserNotifyPropsImportData {

	getProp := func(key string) *string {
		if v, ok := notifyProps[key]; ok {
			return &v
		}
		return nil
	}

	return &imports.UserNotifyPropsImportData{
		Desktop:          getProp(model.DesktopNotifyProp),
		DesktopSound:     getProp(model.DesktopSoundNotifyProp),
		Email:            getProp(model.EmailNotifyProp),
		Mobile:           getProp(model.PushNotifyProp),
		MobilePushStatus: getProp(model.PushStatusNotifyProp),
		ChannelTrigger:   getProp(model.ChannelMentionsNotifyProp),
		CommentsTrigger:  getProp(model.CommentsNotifyProp),
		MentionKeys:      getProp(model.MentionKeysNotifyProp),
	}
}

func (a *App) exportAllPosts(ctx request.CTX, writer io.Writer, withAttachments bool) ([]imports.AttachmentImportData, *model.AppError) {
	var attachments []imports.AttachmentImportData
	afterId := strings.Repeat("0", 26)

	for {
		posts, nErr := a.Srv().Store().Post().GetParentsForExportAfter(1000, afterId)
		if nErr != nil {
			return nil, model.NewAppError("exportAllPosts", "app.post.get_posts.app_error", nil, "", http.StatusInternalServerError).Wrap(nErr)
		}

		if len(posts) == 0 {
			return attachments, nil
		}

		for _, post := range posts {
			afterId = post.Id

			// Skip deleted.
			if post.DeleteAt != 0 {
				continue
			}

			postLine := ImportLineForPost(post)

			replies, replyAttachments, err := a.buildPostReplies(ctx, post.Id, withAttachments)
			if err != nil {
				return nil, err
			}

			if withAttachments && len(replyAttachments) > 0 {
				attachments = append(attachments, replyAttachments...)
			}

			postLine.Post.Replies = &replies
			postLine.Post.Reactions = &[]imports.ReactionImportData{}
			if post.HasReactions {
				postLine.Post.Reactions, err = a.BuildPostReactions(ctx, post.Id)
				if err != nil {
					return nil, err
				}
			}

			if len(post.FileIds) > 0 {
				postAttachments, err := a.buildPostAttachments(post.Id)
				if err != nil {
					return nil, err
				}
				postLine.Post.Attachments = &postAttachments

				if withAttachments && len(postAttachments) > 0 {
					attachments = append(attachments, postAttachments...)
				}
			}

			if err := a.exportWriteLine(writer, postLine); err != nil {
				return nil, err
			}
		}
	}
}

func (a *App) buildPostReplies(ctx request.CTX, postID string, withAttachments bool) ([]imports.ReplyImportData, []imports.AttachmentImportData, *model.AppError) {
	var replies []imports.ReplyImportData
	var attachments []imports.AttachmentImportData

	replyPosts, nErr := a.Srv().Store().Post().GetRepliesForExport(postID)
	if nErr != nil {
		return nil, nil, model.NewAppError("buildPostReplies", "app.post.get_posts.app_error", nil, "", http.StatusInternalServerError).Wrap(nErr)
	}

	for _, reply := range replyPosts {
		replyImportObject := ImportReplyFromPost(reply)
		if reply.HasReactions {
			var appErr *model.AppError
			replyImportObject.Reactions, appErr = a.BuildPostReactions(ctx, reply.Id)
			if appErr != nil {
				return nil, nil, appErr
			}
		}
		if len(reply.FileIds) > 0 {
			postAttachments, appErr := a.buildPostAttachments(reply.Id)
			if appErr != nil {
				return nil, nil, appErr
			}
			replyImportObject.Attachments = &postAttachments
			if withAttachments && len(postAttachments) > 0 {
				attachments = append(attachments, postAttachments...)
			}
		}

		replies = append(replies, *replyImportObject)
	}

	return replies, attachments, nil
}

func (a *App) BuildPostReactions(ctx request.CTX, postID string) (*[]ReactionImportData, *model.AppError) {
	var reactionsOfPost []imports.ReactionImportData

	reactions, nErr := a.Srv().Store().Reaction().GetForPost(postID, true)
	if nErr != nil {
		return nil, model.NewAppError("BuildPostReactions", "app.reaction.get_for_post.app_error", nil, "", http.StatusInternalServerError).Wrap(nErr)
	}

	for _, reaction := range reactions {
		user, err := a.Srv().Store().User().Get(context.Background(), reaction.UserId)
		if err != nil {
			var nfErr *store.ErrNotFound
			if errors.As(err, &nfErr) { // this is a valid case, the user that reacted might've been deleted by now
				ctx.Logger().Info("Skipping reactions by user since the entity doesn't exist anymore", mlog.String("user_id", reaction.UserId))
				continue
			}
			return nil, model.NewAppError("BuildPostReactions", "app.user.get.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		}
		reactionsOfPost = append(reactionsOfPost, *ImportReactionFromPost(user, reaction))
	}

	return &reactionsOfPost, nil

}

func (a *App) buildPostAttachments(postID string) ([]imports.AttachmentImportData, *model.AppError) {
	infos, nErr := a.Srv().Store().FileInfo().GetForPost(postID, false, false, false)
	if nErr != nil {
		return nil, model.NewAppError("buildPostAttachments", "app.file_info.get_for_post.app_error", nil, "", http.StatusInternalServerError).Wrap(nErr)
	}

	attachments := make([]imports.AttachmentImportData, 0, len(infos))
	for _, info := range infos {
		attachments = append(attachments, imports.AttachmentImportData{Path: &info.Path})
	}

	return attachments, nil
}

func (a *App) exportCustomEmoji(writer io.Writer, outPath, exportDir string, exportFiles bool) ([]string, *model.AppError) {
	var emojiPaths []string
	pageNumber := 0
	for {
		customEmojiList, err := a.GetEmojiList(pageNumber, 100, model.EmojiSortByName)

		if err != nil {
			return nil, err
		}

		if len(customEmojiList) == 0 {
			break
		}

		pageNumber++

		emojiPath := filepath.Join(*a.Config().FileSettings.Directory, "emoji")
		pathToDir := filepath.Join(outPath, exportDir)
		if exportFiles {
			if _, err := os.Stat(pathToDir); os.IsNotExist(err) {
				os.Mkdir(pathToDir, os.ModePerm)
			}
		}

		for _, emoji := range customEmojiList {
			emojiImagePath := filepath.Join(emojiPath, emoji.Id, "image")
			filePath := filepath.Join(exportDir, emoji.Id, "image")
			if exportFiles {
				err := a.copyEmojiImages(emoji.Id, emojiImagePath, pathToDir)
				if err != nil {
					return nil, model.NewAppError("BulkExport", "app.export.export_custom_emoji.copy_emoji_images.error", nil, "err="+err.Error(), http.StatusBadRequest)
				}
			} else {
				filePath = filepath.Join("emoji", emoji.Id, "image")
				emojiPaths = append(emojiPaths, filePath)
			}

			emojiImportObject := ImportLineFromEmoji(emoji, filePath)
			if err := a.exportWriteLine(writer, emojiImportObject); err != nil {
				return nil, err
			}
		}
	}

	return emojiPaths, nil
}

// Copies emoji files from 'data/emoji' dir to 'exported_emoji' dir
func (a *App) copyEmojiImages(emojiId string, emojiImagePath string, pathToDir string) error {
	fromPath, err := os.Open(emojiImagePath)
	if fromPath == nil || err != nil {
		return errors.New("Error reading " + emojiImagePath + "file")
	}
	defer fromPath.Close()

	emojiDir := pathToDir + "/" + emojiId

	if _, err = os.Stat(emojiDir); err != nil {
		if !os.IsNotExist(err) {
			return errors.Wrapf(err, "Error fetching file info of emoji directory %v", emojiDir)
		}

		if err = os.Mkdir(emojiDir, os.ModePerm); err != nil {
			return errors.Wrapf(err, "Error creating emoji directory %v", emojiDir)
		}
	}

	toPath, err := os.OpenFile(emojiDir+"/image", os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return errors.New("Error creating the image file " + err.Error())
	}
	defer toPath.Close()

	_, err = io.Copy(toPath, fromPath)
	if err != nil {
		return errors.New("Error copying emojis " + err.Error())
	}

	return nil
}

func (a *App) exportAllDirectChannels(writer io.Writer) *model.AppError {
	afterId := strings.Repeat("0", 26)
	for {
		channels, err := a.Srv().Store().Channel().GetAllDirectChannelsForExportAfter(1000, afterId)
		if err != nil {
			return model.NewAppError("exportAllDirectChannels", "app.channel.get_all_direct.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		}

		if len(channels) == 0 {
			break
		}

		for _, channel := range channels {
			afterId = channel.Id

			// Skip if there are no active members in the channel
			if len(*channel.Members) == 0 {
				continue
			}

			// Skip deleted.
			if channel.DeleteAt != 0 {
				continue
			}

			channelLine := ImportLineFromDirectChannel(channel)
			if err := a.exportWriteLine(writer, channelLine); err != nil {
				return err
			}
		}
	}

	return nil
}

func (a *App) exportAllDirectPosts(ctx request.CTX, writer io.Writer, withAttachments bool) ([]imports.AttachmentImportData, *model.AppError) {
	var attachments []imports.AttachmentImportData
	afterId := strings.Repeat("0", 26)
	for {
		posts, err := a.Srv().Store().Post().GetDirectPostParentsForExportAfter(1000, afterId)
		if err != nil {
			return nil, model.NewAppError("exportAllDirectPosts", "app.post.get_direct_posts.app_error", nil, "", http.StatusInternalServerError).Wrap(err)
		}

		if len(posts) == 0 {
			break
		}

		for _, post := range posts {
			afterId = post.Id

			// Skip deleted.
			if post.DeleteAt != 0 {
				continue
			}

			// Handle attachments.
			var postAttachments []imports.AttachmentImportData
			var err *model.AppError
			if len(post.FileIds) > 0 {
				postAttachments, err = a.buildPostAttachments(post.Id)
				if err != nil {
					return nil, err
				}

				if withAttachments && len(postAttachments) > 0 {
					attachments = append(attachments, postAttachments...)
				}
			}

			// Do the Replies.
			replies, replyAttachments, err := a.buildPostReplies(ctx, post.Id, withAttachments)
			if err != nil {
				return nil, err
			}

			if withAttachments && len(replyAttachments) > 0 {
				attachments = append(attachments, replyAttachments...)
			}

			postLine := ImportLineForDirectPost(post)
			postLine.DirectPost.Replies = &replies
			if len(postAttachments) > 0 {
				postLine.DirectPost.Attachments = &postAttachments
			}
			if err := a.exportWriteLine(writer, postLine); err != nil {
				return nil, err
			}
		}
	}
	return attachments, nil
}

func (a *App) exportFile(outPath, filePath string, zipWr *zip.Writer) *model.AppError {
	var wr io.Writer
	var err error
	rd, appErr := a.FileReader(filePath)
	if appErr != nil {
		return appErr
	}
	defer rd.Close()

	if zipWr != nil {
		wr, err = zipWr.CreateHeader(&zip.FileHeader{
			Name:   filepath.Join(model.ExportDataDir, filePath),
			Method: zip.Store,
		})
		if err != nil {
			return model.NewAppError("exportFileAttachment", "app.export.export_attachment.zip_create_header.error",
				nil, "err="+err.Error(), http.StatusInternalServerError)
		}
	} else {
		filePath = filepath.Join(outPath, model.ExportDataDir, filePath)
		if err = os.MkdirAll(filepath.Dir(filePath), 0700); err != nil {
			return model.NewAppError("exportFileAttachment", "app.export.export_attachment.mkdirall.error",
				nil, "err="+err.Error(), http.StatusInternalServerError)
		}

		wr, err = os.Create(filePath)
		if err != nil {
			return model.NewAppError("exportFileAttachment", "app.export.export_attachment.create_file.error",
				nil, "err="+err.Error(), http.StatusInternalServerError)
		}
		defer wr.(*os.File).Close()
	}

	if _, err := io.Copy(wr, rd); err != nil {
		return model.NewAppError("exportFileAttachment", "app.export.export_attachment.copy_file.error",
			nil, "err="+err.Error(), http.StatusInternalServerError)
	}

	return nil
}

func (a *App) ListExports() ([]string, *model.AppError) {
	exports, appErr := a.ListDirectory(*a.Config().ExportSettings.Directory)
	if appErr != nil {
		return nil, appErr
	}

	results := make([]string, len(exports))
	for i := range exports {
		results[i] = filepath.Base(exports[i])
	}

	return results, nil
}

func (a *App) DeleteExport(name string) *model.AppError {
	filePath := filepath.Join(*a.Config().ExportSettings.Directory, name)

	if ok, err := a.FileExists(filePath); err != nil {
		return err
	} else if !ok {
		return nil
	}

	return a.RemoveFile(filePath)
}
