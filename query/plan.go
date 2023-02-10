package query

import (
	"bytes"
	"container/heap"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type FinalPlan interface {
	String() string
	Explain() []string
	Init() error
	Next() ([]Column, error)
	FieldNameList() []string
}

type Plan interface {
	String() string
	Explain() []string
	Init() error
	Next() (key []byte, value []byte, err error)
}

var (
	_ Plan = (*FullScanPlan)(nil)
	_ Plan = (*EmptyResultPlan)(nil)
	_ Plan = (*RangeScanPlan)(nil)
	_ Plan = (*PrefixScanPlan)(nil)
	_ Plan = (*MultiGetPlan)(nil)
	_ Plan = (*LimitPlan)(nil)
	_ Plan = (*OrderPlan)(nil)

	_ FinalPlan = (*ProjectionPlan)(nil)
	_ FinalPlan = (*AggregatePlan)(nil)
	_ FinalPlan = (*FinalOrderPlan)(nil)
	_ FinalPlan = (*FinalLimitPlan)(nil)
)

type Column []byte

type FullScanPlan struct {
	Txn    Txn
	Filter *FilterExec
	iter   Cursor
}

func NewFullScanPlan(t Txn, f *FilterExec) Plan {
	return &FullScanPlan{
		Txn:    t,
		Filter: f,
	}
}

func (p *FullScanPlan) String() string {
	return fmt.Sprintf("FullScanPlan{Filter = '%s'}", p.Filter.Explain())
}

func (p *FullScanPlan) Explain() []string {
	return []string{p.String()}
}

func (p *FullScanPlan) Init() (err error) {
	p.iter, err = p.Txn.Cursor()
	if err != nil {
		return err
	}
	return p.iter.Seek([]byte{})
}

func (p *FullScanPlan) Next() ([]byte, []byte, error) {
	for {
		key, val, err := p.iter.Next()
		if err != nil {
			return nil, nil, err
		}
		if key == nil {
			break
		}
		ok, err := p.Filter.Filter(NewKVP(key, val))
		if err != nil {
			return nil, nil, err
		}
		if ok {
			return key, val, nil
		}
	}
	return nil, nil, nil
}

type EmptyResultPlan struct {
	Txn Txn
}

func NewEmptyResultPlan(t Txn, f *FilterExec) Plan {
	return &EmptyResultPlan{
		Txn: t,
	}
}

func (p *EmptyResultPlan) Init() error {
	return nil
}

func (p *EmptyResultPlan) Next() ([]byte, []byte, error) {
	return nil, nil, nil
}

func (p *EmptyResultPlan) String() string {
	return "EmptyResultPlan"
}

func (p *EmptyResultPlan) Explain() []string {
	return []string{p.String()}
}

type PrefixScanPlan struct {
	Txn    Txn
	Filter *FilterExec
	Prefix string
	iter   Cursor
}

func NewPrefixScanPlan(t Txn, f *FilterExec, p string) Plan {
	return &PrefixScanPlan{
		Txn:    t,
		Filter: f,
		Prefix: p,
	}
}

func (p *PrefixScanPlan) Init() (err error) {
	p.iter, err = p.Txn.Cursor()
	if err != nil {
		return err
	}
	return p.iter.Seek([]byte(p.Prefix))
}

func (p *PrefixScanPlan) Next() ([]byte, []byte, error) {
	pb := []byte(p.Prefix)
	for {
		key, val, err := p.iter.Next()
		if err != nil {
			return nil, nil, err
		}
		if key == nil {
			break
		}

		// Key not have the prefix
		if !bytes.HasPrefix(key, pb) {
			break
		}

		// Filter with the expression
		ok, err := p.Filter.Filter(NewKVP(key, val))
		if err != nil {
			return nil, nil, err
		}
		if ok {
			return key, val, nil
		}
	}
	return nil, nil, nil
}

func (p *PrefixScanPlan) String() string {
	return fmt.Sprintf("PrefixScanPlan{Prefix = '%s', Filter = '%s'}", p.Prefix, p.Filter.Explain())
}

func (p *PrefixScanPlan) Explain() []string {
	return []string{p.String()}
}

type RangeScanPlan struct {
	Txn    Txn
	Filter *FilterExec
	Start  []byte
	End    []byte
	iter   Cursor
}

func NewRangeScanPlan(t Txn, f *FilterExec, start []byte, end []byte) Plan {
	return &RangeScanPlan{
		Txn:    t,
		Filter: f,
		Start:  start,
		End:    end,
	}
}

func (p *RangeScanPlan) Init() (err error) {
	p.iter, err = p.Txn.Cursor()
	if err != nil {
		return err
	}
	if p.Start != nil {
		err = p.iter.Seek(p.Start)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *RangeScanPlan) Next() ([]byte, []byte, error) {
	for {
		key, val, err := p.iter.Next()
		if err != nil {
			return nil, nil, err
		}
		if key == nil {
			break
		}

		// Key is greater than End
		if p.End != nil && bytes.Compare(key, p.End) > 0 {
			break
		}

		// Filter with the expression
		ok, err := p.Filter.Filter(NewKVP(key, val))
		if err != nil {
			return nil, nil, err
		}
		if ok {
			return key, val, nil
		}
	}
	return nil, nil, nil
}

func convertByteToString(val []byte) string {
	if val == nil {
		return "<nil>"
	}
	return string(val)
}

func (p *RangeScanPlan) String() string {
	return fmt.Sprintf("RangeScanPlan{Start = '%s', End = '%s', Filter = '%s'}", convertByteToString(p.Start), convertByteToString(p.End), p.Filter.Explain())
}

func (p *RangeScanPlan) Explain() []string {
	return []string{p.String()}
}

type MultiGetPlan struct {
	Txn     Txn
	Filter  *FilterExec
	Keys    []string
	numKeys int
	idx     int
}

func NewMultiGetPlan(t Txn, f *FilterExec, keys []string) Plan {
	// We should sort keys to ensure order by erase works correctly
	sort.Strings(keys)
	return &MultiGetPlan{
		Txn:     t,
		Filter:  f,
		Keys:    keys,
		idx:     0,
		numKeys: len(keys),
	}
}

func (p *MultiGetPlan) Init() error {
	return nil
}

func (p *MultiGetPlan) Next() ([]byte, []byte, error) {
	for {
		if p.idx >= p.numKeys {
			break
		}
		key := []byte(p.Keys[p.idx])
		p.idx++
		val, err := p.Txn.Get(key)
		if err != nil {
			return nil, nil, err
		}
		if val == nil {
			// No Value
			continue
		}
		ok, err := p.Filter.Filter(NewKVP(key, val))
		if err != nil {
			return nil, nil, err
		}
		if ok {
			return key, val, nil
		}
	}
	return nil, nil, nil
}

func (p *MultiGetPlan) String() string {
	keys := strings.Join(p.Keys, ", ")
	return fmt.Sprintf("MultiGetPlan{Keys = <%s>, Filter = '%s'}", keys, p.Filter.Explain())
}

func (p *MultiGetPlan) Explain() []string {
	return []string{p.String()}
}

type LimitPlan struct {
	Txn       Txn
	Start     int
	Count     int
	current   int
	skips     int
	ChildPlan Plan
}

func (p *LimitPlan) Init() error {
	p.current = 0
	p.skips = 0
	return p.ChildPlan.Init()
}

func (p *LimitPlan) Next() ([]byte, []byte, error) {
	for p.skips < p.Start {
		k, v, err := p.ChildPlan.Next()
		if err != nil {
			return nil, nil, err
		}
		if k == nil && v == nil && err == nil {
			return nil, nil, nil
		}
		p.skips++
	}
	if p.current >= p.Count {
		return nil, nil, nil
	}
	k, v, err := p.ChildPlan.Next()
	if err != nil {
		return nil, nil, err
	}
	if k == nil && v == nil && err == nil {
		return nil, nil, nil
	}

	p.current++
	return k, v, nil

}

func (p *LimitPlan) String() string {
	return fmt.Sprintf("LimitPlan{Start = %d, Count = %d}", p.Start, p.Count)
}

func (p *LimitPlan) Explain() []string {
	ret := []string{p.String()}
	for _, plan := range p.ChildPlan.Explain() {
		ret = append(ret, plan)
	}
	return ret
}

type orderRow struct {
	key    []byte
	value  []byte
	orders []OrderField
}

func (l *orderRow) Less(r *orderRow) bool {
	lkv := NewKVP(l.key, l.value)
	rkv := NewKVP(r.key, r.value)
	for _, order := range l.orders {
		var compare int
		lval, _ := order.Field.Execute(lkv)
		rval, _ := order.Field.Execute(rkv)
		switch lval.(type) {
		case int, int8, int16, int32, int64, uint, uint16, uint32, uint64:
			compare = l.compareInt(lval, rval, order.Order == DESC)
		case float32, float64:
			compare = l.compareFloat(lval, rval, order.Order == DESC)
		case []byte, string:
			compare = l.compareBytes(lval, rval, order.Order == DESC)
		case bool:
			compare = l.compareBool(lval, rval, order.Order == DESC)
		default:
			return false
		}
		if compare < 0 {
			return true
		} else if compare > 0 {
			return false
		}
	}
	return false
}

func (l *orderRow) compareBool(lval, rval any, reverse bool) int {
	lbool := lval.(bool)
	rbool := rval.(bool)
	lint := 0
	rint := 0
	if lbool {
		lint = 1
	}
	if rbool {
		rint = 1
	}
	if lint == rint {
		return 0
	}
	if reverse {
		if lint > rint {
			return -1
		} else {
			return 1
		}
	}
	if lint < rint {
		return -1
	} else {
		return 1
	}
}

func (l *orderRow) compareInt(lval, rval any, reverse bool) int {
	lint, _ := convertToInt(lval)
	rint, _ := convertToInt(rval)
	if lint == rint {
		return 0
	}
	if reverse {
		if lint > rint {
			return -1
		} else {
			return 1
		}
	}
	if lint < rint {
		return -1
	} else {
		return 1
	}
}

func (l *orderRow) compareFloat(lval, rval any, reverse bool) int {
	lf, _ := convertToFloat(lval)
	rf, _ := convertToFloat(rval)
	if lf == rf {
		return 0
	}
	if reverse {
		if lf > rf {
			return -1
		} else {
			return 1
		}
	}
	if lf < rf {
		return -1
	} else {
		return 1
	}
}

func (l *orderRow) compareBytes(lval, rval any, reverse bool) int {
	lb, _ := convertToByteArray(lval)
	rb, _ := convertToByteArray(rval)
	if reverse {
		return 0 - bytes.Compare(lb, rb)
	}
	return bytes.Compare(lb, rb)
}

type orderRowHeap []*orderRow

func (h orderRowHeap) Len() int {
	return len(h)
}

func (h orderRowHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *orderRowHeap) Push(x any) {
	*h = append(*h, x.(*orderRow))
}

func (h *orderRowHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

func (h orderRowHeap) Less(i, j int) bool {
	l := h[i]
	r := h[j]
	return l.Less(r)
}

type OrderPlan struct {
	Txn       Txn
	Orders    []OrderField
	ChildPlan Plan
	pos       int
	total     int
	sorted    *orderRowHeap
}

func (p *OrderPlan) Init() error {
	p.pos = 0
	p.total = 0
	p.sorted = &orderRowHeap{}
	heap.Init(p.sorted)
	p.ChildPlan.Init()
	return nil
}

func (p *OrderPlan) prepare() error {
	for {
		k, v, err := p.ChildPlan.Next()
		if err != nil {
			return err
		}
		// Take all data
		if k == nil && v == nil && err == nil {
			break
		}
		row := &orderRow{
			key:    k,
			value:  v,
			orders: p.Orders,
		}
		heap.Push(p.sorted, row)
		p.total++
	}
	return nil
}

func (p *OrderPlan) Next() ([]byte, []byte, error) {
	if p.total == 0 {
		if err := p.prepare(); err != nil {
			return nil, nil, err
		}
	}
	if p.pos < p.total {
		rrow := heap.Pop(p.sorted)
		row := rrow.(*orderRow)
		p.pos++
		return row.key, row.value, nil
	}
	return nil, nil, nil
}

func (p *OrderPlan) String() string {
	fields := []string{}
	for _, f := range p.Orders {
		orderStr := " ASC"
		if f.Order == DESC {
			orderStr = " DESC"
		}
		fields = append(fields, f.Field.String()+orderStr)
	}

	return fmt.Sprintf("OrderPlan{Fields = <%s>}", strings.Join(fields, ", "))
}

func (p *OrderPlan) Explain() []string {
	ret := []string{p.String()}
	for _, plan := range p.ChildPlan.Explain() {
		ret = append(ret, plan)
	}
	return ret
}

type FinalOrderPlan struct {
	Txn        Txn
	Orders     []OrderField
	FieldNames []string
	ChildPlan  FinalPlan
	pos        int
	total      int
	sorted     *orderColumnsRowHeap
	orderPos   []int
	orderTypes []Type
}

func (p *FinalOrderPlan) findOrderIdx(o OrderField) (int, error) {
	fname := o.Name
	for i, fn := range p.FieldNames {
		if fname == fn {
			return i, nil
		}
	}
	return 0, fmt.Errorf("Cannot find field: %s", fname)
}

func (p *FinalOrderPlan) Init() error {
	p.pos = 0
	p.total = 0
	p.orderPos = []int{}
	p.orderTypes = []Type{}
	for _, o := range p.Orders {
		idx, err := p.findOrderIdx(o)
		if err != nil {
			return err
		}
		p.orderPos = append(p.orderPos, idx)
		p.orderTypes = append(p.orderTypes, o.Field.ReturnType())
	}
	p.sorted = &orderColumnsRowHeap{}
	heap.Init(p.sorted)
	p.ChildPlan.Init()
	return nil
}

func (p *FinalOrderPlan) FieldNameList() []string {
	return p.FieldNames
}

func (p *FinalOrderPlan) String() string {
	fields := []string{}
	for _, f := range p.Orders {
		orderStr := " ASC"
		if f.Order == DESC {
			orderStr = " DESC"
		}
		fields = append(fields, f.Name+orderStr)
	}
	return fmt.Sprintf("OrderPlan{Fields = <%s>}", strings.Join(fields, ", "))
}

func (p *FinalOrderPlan) Explain() []string {
	ret := []string{p.String()}
	for _, plan := range p.ChildPlan.Explain() {
		ret = append(ret, plan)
	}
	return ret
}

func (p *FinalOrderPlan) Next() ([]Column, error) {
	if p.total == 0 {
		if err := p.prepare(); err != nil {
			return nil, err
		}
	}
	if p.pos < p.total {
		rrow := heap.Pop(p.sorted)
		row := rrow.(*orderColumnsRow)
		p.pos++
		return row.cols, nil
	}
	return nil, nil
}

func (p *FinalOrderPlan) prepare() error {
	for {
		col, err := p.ChildPlan.Next()
		if err != nil {
			return err
		}
		if col == nil && err == nil {
			break
		}
		row := &orderColumnsRow{
			cols:       col,
			orders:     p.Orders,
			orderPos:   p.orderPos,
			orderTypes: p.orderTypes,
		}
		heap.Push(p.sorted, row)
		p.total++
	}
	return nil
}

type orderColumnsRow struct {
	cols       []Column
	orders     []OrderField
	orderPos   []int
	orderTypes []Type
}

func (l *orderColumnsRow) Less(r *orderColumnsRow) bool {
	for i, o := range l.orders {
		oidx := l.orderPos[i]
		desc := o.Order == DESC
		var compare int
		lval := l.cols[oidx]
		rval := r.cols[oidx]
		switch l.orderTypes[i] {
		case TSTR:
			compare = l.compareBytes(lval, rval, desc)
		case TBOOL:
			compare = l.compareBool(lval, rval, desc)
		case TNUMBER:
			compare = l.compareNumber(lval, rval, desc)
		default:
			return false
		}
		if compare < 0 {
			return true
		} else if compare > 0 {
			return false
		}
	}
	return false
}

func (l *orderColumnsRow) compareBytes(lval, rval []byte, reverse bool) int {
	if reverse {
		return 0 - bytes.Compare(lval, rval)
	}
	return bytes.Compare(lval, rval)
}

func (l *orderColumnsRow) compareBool(lval, rval []byte, reverse bool) int {
	lbool := bytes.Equal(lval, []byte("true"))
	rbool := bytes.Equal(rval, []byte("true"))
	lint := 0
	rint := 0
	if lbool {
		lint = 1
	}
	if rbool {
		rint = 1
	}
	if lint == rint {
		return 0
	}
	if reverse {
		if lint > rint {
			return -1
		} else {
			return 1
		}
	}
	if lint < rint {
		return -1
	} else {
		return 1
	}
}

func (l *orderColumnsRow) compareNumber(lval, rval []byte, reverse bool) int {
	var (
		lint, rint     int64
		lfloat, rfloat float64
		err            error
	)
	if lint, err = strconv.ParseInt(string(lval), 10, 64); err == nil {
		if rint, err = strconv.ParseInt(string(rval), 10, 64); err == nil {
			return l.compareInt(lint, rint, reverse)
		}
	}

	if lfloat, err = strconv.ParseFloat(string(lval), 64); err == nil {
		if rfloat, err = strconv.ParseFloat(string(rval), 64); err == nil {
			return l.compareFloat(lfloat, rfloat, reverse)
		}
	}
	return 0
}

func (l *orderColumnsRow) compareInt(lval, rval int64, reverse bool) int {
	if lval == rval {
		return 0
	}
	if reverse {
		if lval > rval {
			return -1
		} else {
			return 1
		}
	}
	if lval < rval {
		return -1
	} else {
		return 1
	}
}

func (l *orderColumnsRow) compareFloat(lval, rval float64, reverse bool) int {
	if lval == rval {
		return 0
	}
	if reverse {
		if lval > rval {
			return -1
		} else {
			return 1
		}
	}
	if lval < rval {
		return -1
	} else {
		return 1
	}
}

type orderColumnsRowHeap []*orderColumnsRow

func (h orderColumnsRowHeap) Len() int {
	return len(h)
}

func (h orderColumnsRowHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *orderColumnsRowHeap) Push(x any) {
	*h = append(*h, x.(*orderColumnsRow))
}

func (h *orderColumnsRowHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

func (h orderColumnsRowHeap) Less(i, j int) bool {
	l := h[i]
	r := h[j]
	return l.Less(r)
}

type FinalLimitPlan struct {
	Txn        Txn
	Start      int
	Count      int
	current    int
	skips      int
	ChildPlan  FinalPlan
	FieldNames []string
}

func (p *FinalLimitPlan) Init() error {
	p.current = 0
	p.skips = 0
	return p.ChildPlan.Init()
}

func (p *FinalLimitPlan) Next() ([]Column, error) {
	for p.skips < p.Start {
		cols, err := p.ChildPlan.Next()
		if err != nil {
			return nil, err
		}
		if cols == nil && err == nil {
			return nil, nil
		}
		p.skips++
	}
	if p.current >= p.Count {
		return nil, nil
	}
	cols, err := p.ChildPlan.Next()
	if err != nil {
		return nil, err
	}
	if cols == nil && err == nil {
		return nil, nil
	}

	p.current++
	return cols, nil

}

func (p *FinalLimitPlan) String() string {
	return fmt.Sprintf("LimitPlan{Start = %d, Count = %d}", p.Start, p.Count)
}

func (p *FinalLimitPlan) Explain() []string {
	ret := []string{p.String()}
	for _, plan := range p.ChildPlan.Explain() {
		ret = append(ret, plan)
	}
	return ret
}

func (p *FinalLimitPlan) FieldNameList() []string {
	return p.FieldNames
}
