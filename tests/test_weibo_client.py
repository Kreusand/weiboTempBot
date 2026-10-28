import unittest


from weibo_client import WeiboClient, collectPhotos, collectVideo, statusTags, cleanHtml, isPinned, sortNewestFirst, videoPageLink


class WeiboClientTests(unittest.TestCase):
    def test_topic_page_is_not_a_video_page(self):
        topic = {"page_info": {"type": "topic", "page_url": "https://m.weibo.cn/p/index?containerid=100808abc"}}
        self.assertEqual(videoPageLink(topic, []), "")

    def test_video_page_is_kept_for_real_videos(self):
        video = {"page_info": {"type": "video", "page_url": "https://video.weibo.com/show?fid=1"}}
        self.assertEqual(videoPageLink(video, []), "https://video.weibo.com/show?fid=1")

    def test_is_pinned_signals(self):
        self.assertTrue(isPinned({"isTop": 1}))
        self.assertTrue(isPinned({"isPinnedCard": True}))
        self.assertTrue(isPinned({"title": {"text": "\u7f6e\u9876"}}))
        self.assertFalse(isPinned({"isTop": 0, "title": {"text": "other"}}))
        self.assertFalse(isPinned({}))

    def test_sort_newest_first_moves_pinned_post_down(self):
        statuses = [{"idstr": "100"}, {"idstr": "300"}, {"idstr": "200"}]
        self.assertEqual([item["idstr"] for item in sortNewestFirst(statuses)], ["300", "200", "100"])

    def test_clean_html(self):
        self.assertEqual(cleanHtml("<span>你好</span><br><span>世界</span>"), "你好 世界")

    def test_collect_photos(self):
        status = {"pics": [{"large": {"url": "https://img.example/a.jpg"}}]}
        media = collectPhotos(status)
        self.assertEqual(len(media), 1)
        self.assertEqual(media[0].kind, "photo")

    def test_collect_video_only_direct_url(self):
        status = {"page_info": {"media_info": {"mp4_hd_url": "https://video.example/a.mp4"}, "page_url": "https://weibo.com/tv/show/123"}}
        self.assertEqual(collectVideo(status)[0].url, "https://video.example/a.mp4")
        self.assertEqual(collectVideo({"page_info": {"page_url": "https://weibo.com/tv/show/123"}}), [])

    def test_tags(self):
        tags = statusTags({"visible": {"type": 1}, "retweeted_status": {"id": "1"}, "pic_num": 1,
                           "page_info": {"type": "video"}}, [])
        self.assertIn("#微博动态", tags)
        self.assertIn("#微博粉丝可见动态", tags)
        self.assertIn("#微博转发", tags)
        self.assertIn("#微博视频", tags)
        self.assertIn("#微博照片", tags)

    def test_comment_list_shape(self):
        payload = {"data": {"data": [{"id": "11"}, {"id": "12"}]}}
        self.assertEqual(len(WeiboClient._extractCommentList(payload)), 2)

    def test_nested_replies_shape(self):
        value = {"comment": [{"id": "22"}, {"id": "23"}]}
        self.assertEqual(len(WeiboClient._nestedReplies(value)), 2)


class CommentEventTests(unittest.IsolatedAsyncioTestCase):
    async def test_only_target_comments_and_replies_on_target_posts(self):
        client = WeiboClient(None, "", 5, 1, 10, 10, 0)
        targetPost = {
            "idstr": "100",
            "bid": "postbid",
            "text": "target post",
            "user": {"idstr": "12345", "screen_name": "Target"},
        }
        otherPost = {
            "idstr": "200",
            "text": "other post",
            "user": {"idstr": "98765", "screen_name": "Other"},
        }
        comments = [
            {
                "idstr": "301",
                "text": "target own comment",
                "user": {"idstr": "12345", "screen_name": "Target"},
                "comments": [],
                "total_number": 0,
            },
            {
                "idstr": "302",
                "text": "someone else's comment",
                "user": {"idstr": "99999", "screen_name": "OtherUser"},
                "comments": [
                    {
                        "idstr": "303",
                        "text": "target reply",
                        "user": {"idstr": "12345", "screen_name": "Target"},
                    }
                ],
                "total_number": 1,
            },
        ]
        seenPosts = []

        async def fakeTopComments(postId):
            seenPosts.append(postId)
            return comments if postId == "100" else []

        async def fakeChildReplies(postId, commentId):
            return []

        client._fetchTopComments = fakeTopComments
        client._fetchChildReplies = fakeChildReplies

        events = await client.fetchCommentEvents("12345", [targetPost, otherPost])

        self.assertEqual(seenPosts, ["100"])
        self.assertEqual(len(events), 2)
        self.assertEqual({event.tags[0] for event in events}, {"#微博评论", "#微博评论回复"})
        self.assertTrue(any("target own comment" in event.summary for event in events))
        self.assertTrue(any("target reply" in event.summary for event in events))
        self.assertFalse(any("someone else's comment" in event.summary for event in events))

    async def test_missing_post_author_id_is_skipped(self):
        client = WeiboClient(None, "", 5, 1, 10, 10, 0)
        unknownPost = {"idstr": "100", "text": "unknown author", "user": {"screen_name": "Target"}}
        seenPosts = []

        async def fakeTopComments(postId):
            seenPosts.append(postId)
            return []

        client._fetchTopComments = fakeTopComments
        events = await client.fetchCommentEvents("12345", [unknownPost])

        self.assertEqual(seenPosts, [])
        self.assertEqual(events, [])


if __name__ == "__main__":
    unittest.main()