package eval

import "testing"

// 聚合层统计数值的语义测试（黄卡①返工）：percentile 已知值对照、
// majoritySuccess 多数决语义、percentile 不就地排序入参。
// 承重墙意义：Task 12 合入判定直接消费这些中位数与成败布尔——
// 假链路明细全为 1 时 mean==median，无语义对照的统计实现会被
// 「数值看起来正常」的假绿放行（median→mean 与 `>`→`>=` 两类
// 变异曾均不转红）。

// TestPercentile_KnownValues 钉死线性插值分位的已知值口径（与 numpy 默认
// linear 一致）。n=2/3/4 三组的 P25/P50/P95 全部对照手算值：
// 任一分位实现漂移（含「整套分位退化成均值」）都会让至少一列转红。
func TestPercentile_KnownValues(t *testing.T) {
	cases := []struct {
		name string
		vals []float64
		q    float64
		want float64
	}{
		{"n=2 [10,20] P25", []float64{10, 20}, 0.25, 12.5},
		{"n=2 [10,20] P50", []float64{10, 20}, 0.50, 15},
		{"n=2 [10,20] P95", []float64{10, 20}, 0.95, 19.5},
		{"n=3 [1,2,3] P25", []float64{1, 2, 3}, 0.25, 1.5},
		{"n=3 [1,2,3] P50", []float64{1, 2, 3}, 0.50, 2},
		{"n=3 [1,2,3] P95", []float64{1, 2, 3}, 0.95, 2.9},
		{"n=4 [1,2,3,4] P25", []float64{1, 2, 3, 4}, 0.25, 1.75},
		{"n=4 [1,2,3,4] P50", []float64{1, 2, 3, 4}, 0.50, 2.5},
		{"n=4 [1,2,3,4] P95", []float64{1, 2, 3, 4}, 0.95, 3.85},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := percentile(c.vals, c.q); !closeEnough(got, c.want) {
				t.Errorf("percentile(%v, %v) = %v, want %v", c.vals, c.q, got, c.want)
			}
		})
	}
	// median 与 mean 必须可区分：上表是对称数据，P50 与均值同值
	//（假链路明细全为 1 时尤其如此），单靠它抓不住 median→mean 变异。
	// 偏序数据上两者不同值；同时钉死「median 即 P50」的单实现口径
	//（分位数集中在 percentile 一处，禁第二套插值）。
	skew := []float64{1, 2, 100}
	if got := median(skew); !closeEnough(got, 2) {
		t.Errorf("median([1,2,100]) = %v, want 2 (mean would be 34.33…)", got)
	}
	if got := median(skew); !closeEnough(got, percentile(skew, 0.50)) {
		t.Errorf("median = %v diverges from P50 = %v (禁第二套插值口径)", got, percentile(skew, 0.50))
	}
}

// TestMajoritySuccess_TieIsNotPass 钉死多数决语义（majorityThreshold 严格大于）：
// repeat=2 一胜一负（0.5）不算过线——平局即未通过，不给「看起来像过了」
// 的中间态；两胜与三跑两胜过线。`>`→`>=` 变异必须让平局用例转红。
func TestMajoritySuccess_TieIsNotPass(t *testing.T) {
	win := TaskResult{Success: true}
	lose := TaskResult{Success: false}
	if ok, rate := majoritySuccess([]TaskResult{win, lose}); ok || !closeEnough(rate, 0.5) {
		t.Errorf("majoritySuccess(一胜一负) = (%v, %v), want (false, 0.5)（平局不过线）", ok, rate)
	}
	if ok, rate := majoritySuccess([]TaskResult{win, win}); !ok || !closeEnough(rate, 1) {
		t.Errorf("majoritySuccess(两胜) = (%v, %v), want (true, 1)", ok, rate)
	}
	if ok, rate := majoritySuccess([]TaskResult{win, lose, win}); !ok || !closeEnough(rate, 2.0/3.0) {
		t.Errorf("majoritySuccess(三跑两胜) = (%v, %v), want (true, 2/3)", ok, rate)
	}
}

// TestPercentile_DoesNotSortInputInPlace 钉死「内部排序副本」纪律：
// results 的顺序是审计对账锚（明细表与逐任务聚合按它渲染），
// percentile 就地排序入参会让调用方看到的顺序被悄悄改写。
func TestPercentile_DoesNotSortInputInPlace(t *testing.T) {
	vals := []float64{30, 10, 20, 5}
	want := []float64{30, 10, 20, 5} // 原顺序快照（乱序输入，被排序过就对不上账）
	if got := percentile(vals, 0.5); !closeEnough(got, 15) {
		t.Errorf("percentile([30,10,20,5], 0.5) = %v, want 15 (sorted 5,10,20,30 → 中点插值)", got)
	}
	for i := range vals {
		if vals[i] != want[i] {
			t.Fatalf("percentile sorted input in place: vals = %v, want %v（顺序是审计锚）", vals, want)
		}
	}
}
