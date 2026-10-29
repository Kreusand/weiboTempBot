package main

// Everything in this file is text that readers of the channels see, so it has to stay Chinese.
// All other source files, including every message to the administrators, are English.
const (
	tagPost        = "#微博动态"
	tagFansOnly    = "#微博粉丝可见动态"
	tagRepost      = "#微博转发"
	tagVideo       = "#微博视频"
	tagPhoto       = "#微博照片"
	tagComment     = "#微博评论"
	tagCommentRepl = "#微博评论回复"

	typeNamePost    = "微博动态"
	typeNameComment = "微博评论"
	typeNameReply   = "微博评论回复"
	typeNameOther   = "微博更新"

	labelTime       = "时间："
	labelParentLink = "对应微博："
	labelOrigin     = "原文："
	labelVideoPage  = "视频页面："

	postTitleSuffix    = " 的微博"
	commentTitleSuffix = " 的评论"
	replyTitleSuffix   = " 的评论回复"

	emptyPostBody    = "（微博正文为空，请查看原文链接。）"
	emptyCommentBody = "（评论内容为空或仅包含表情）"

	repostHeading       = "转发内容 @"
	repostDefaultAuthor = "原作者"
	replyToPrefix       = "回复 @"
	defaultParentName   = "用户"
	parentPostPrefix    = "对应微博："

	// pinnedLabel is the text Weibo attaches to a pinned post.
	pinnedLabel = "置顶"

	// Notes that are only added for test destinations.
	noteMediaDownloadFailed = "部分媒体无法下载："
	noteMediaNotUploaded    = "以下媒体未能上传，请从原文或链接查看："
)
