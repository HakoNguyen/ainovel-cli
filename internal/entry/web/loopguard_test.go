package web

import (
	"strings"
	"testing"
)

func feedAll(g *loopGuard, text string, chunk int) bool {
	for i := 0; i < len(text); i += chunk {
		end := i + chunk
		if end > len(text) {
			end = len(text)
		}
		if tripped, _ := g.feed(text[i:end]); tripped {
			return true
		}
	}
	return false
}

func TestLoopGuardTripsOnShortRepetition(t *testing.T) {
	var g loopGuard
	text := "Tiền đề: Lâm Hữu Sinh tỉnh dậy sau bếp thuốc. " + strings.Repeat(`"the `, 400)
	if !feedAll(&g, text, 7) {
		t.Fatal("phải phát hiện lặp `\"the `")
	}
	if tripped, _ := g.feed(`"the `); tripped {
		t.Fatal("chỉ được báo một lần cho tới khi reset")
	}
	g.reset()
	if feedAll(&g, "Câu văn mới hoàn toàn bình thường.", 5) {
		t.Fatal("sau reset không được báo nhầm")
	}
}

func TestLoopGuardIgnoresNormalOutput(t *testing.T) {
	cases := map[string]string{
		"văn xuôi": strings.Repeat("Mùi thuốc và mùi máu quyện vào nhau. Cô đếm từng hơi thở của đứa trẻ, rồi đếm lại. ", 3) +
			"Ngoài sân, Lưu Đại quát tháo đám tiểu đồng; tiếng chổi tre quét lá khô vang lên đều đặn trên nền đá. " +
			"A Hoa cúi đầu, giấu bàn tay đang run vào tay áo, và tự nhủ rằng ba mươi năm nghề y không cho phép cô run.",
		"json công cụ": `{"type":"premise","content":"## Tiền đề\n- Thế giới hậu tận thế, linh khí cạn kiệt.\n- Tông môn sống nhờ lõi năng lượng cũ.\n",` +
			`"characters":[{"name":"Tần Mặc","role":"nam chính"},{"name":"Lạc Yên","role":"phản diện"},{"name":"Hứa Quân","role":"quan chức"}],` +
			`"world_rules":[{"rule":"Linh khí chỉ còn ở các mỏ sâu","cost":"tuổi thọ"},{"rule":"Cấy ghép máy làm hỏng kinh mạch","cost":"cảnh giới"}]}`,
		"danh sách markdown": strings.Repeat("- Chương mới: nhân vật gặp thử thách khác nhau ở mỗi cảnh, nhịp tăng dần.\n", 2) +
			"- Chương 3: phản bội.\n- Chương 4: trốn chạy qua khu ổ chuột.\n- Chương 5: lần đầu dùng khí tu giữa thành phố neon.\n",
	}
	for name, text := range cases {
		var g loopGuard
		// lặp lại vài lần để vượt cửa sổ kiểm tra nhưng vẫn là nội dung không tuần hoàn ngắn
		if feedAll(&g, strings.Repeat(text, 4), 11) {
			t.Fatalf("%s: báo lặp nhầm", name)
		}
	}
}
