package retrieve

import "testing"

func TestAnswerCacheKeyDistinguishesFields(t *testing.T) {
	var c AnswerCache
	k1 := c.key("q", "intent", "model", "evidence")
	k2 := c.key("qi", "ntent", "model", "evidence") // same concat, different split
	if k1 == k2 {
		t.Error("cache key collided across field boundaries — length prefixing failed")
	}
}
