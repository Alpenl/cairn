package alloc

import "testing"

// TestHintBoundsRequestDrivenCapacity 锁住「按请求参数预分配」的上限。
//
// ⚠ 期望值一律写**字面量**，不要写成 MaxPrealloc。
//
// 初版把用例写成 `{MaxPrealloc, MaxPrealloc}` / `{MaxPrealloc + 1, MaxPrealloc}`
// ——用被测常量表达期望，对该常量的**取值**恒真。实测把 MaxPrealloc 改成
// 1<<30（等于废掉夹取），测试照样绿。它守住了夹取的形状，守不住夹取的数值，
// 而数值才是这个函数的全部安全属性。
func TestHintBoundsRequestDrivenCapacity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   int
		want int
	}{
		{"负数归零", -1, 0},
		{"极端负数归零", -1 << 30, 0},
		{"零保持", 0, 0},
		{"常规值原样通过", 50, 50},
		{"上限本身原样通过", 1024, 1024},
		{"刚超出上限被夹住", 1025, 1024},
		{"极端值被夹住", 1 << 30, 1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Hint(tc.in); got != tc.want {
				t.Fatalf("Hint(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestMaxPreallocValue 单独钉住常量本身。
//
// 上面的用例已用字面量覆盖边界，这条是显式冗余：改动 MaxPrealloc 时报错信息
// 直接说明「这个值是安全属性的一部分」，而不是让人去猜某个边界用例为什么红。
func TestMaxPreallocValue(t *testing.T) {
	t.Parallel()
	if MaxPrealloc != 1024 {
		t.Fatalf("MaxPrealloc = %d, want 1024——这个值是「请求参数不能驱动任意大分配」"+
			"这条属性的全部内容。确要改动，请同时复核 internal/service 的分页上界"+
			"（TestServicePageLimitsFitPreallocBound 守着它们）。", MaxPrealloc)
	}
}
