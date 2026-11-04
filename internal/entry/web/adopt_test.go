package web

import "testing"

func TestChapterContentFromMine(t *testing.T) {
	ai := "# Chương 1: Mùi thuốc và mùi máu\n\nĐoạn AI viết.\n"
	cases := []struct{ name, mine, want string }{
		{"giữ tiêu đề AI khi tác giả không viết tiêu đề",
			"Đoạn một của tôi.\nĐoạn hai của tôi.\n", "# Chương 1: Mùi thuốc và mùi máu\n\nĐoạn một của tôi.\n\nĐoạn hai của tôi.\n"},
		{"tác giả gõ lại đúng tiêu đề không có dấu #",
			"Chương 1: Mùi thuốc và mùi máu\n\nĐoạn một.", "# Chương 1: Mùi thuốc và mùi máu\n\nĐoạn một.\n"},
		{"tác giả tự đặt tiêu đề markdown",
			"# Chương 1: Tên của tôi\nĐoạn một.", "# Chương 1: Tên của tôi\n\nĐoạn một.\n"},
		{"bỏ dòng trống thừa và CRLF", "\r\n  Đoạn một.  \r\n\r\n\r\nĐoạn hai.\r\n", "# Chương 1: Mùi thuốc và mùi máu\n\nĐoạn một.\n\nĐoạn hai.\n"},
		{"bản trống", "  \n\n", ""},
	}
	for _, c := range cases {
		if got := chapterContentFromMine(c.mine, ai); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
	if got := chapterContentFromMine("Đoạn một.", "Không có tiêu đề markdown.\n"); got != "Đoạn một.\n" {
		t.Errorf("AI không có tiêu đề thì không thêm gì, got %q", got)
	}
}
