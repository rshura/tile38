package collection

import (
	"runtime"

	"github.com/tidwall/btree"
	"github.com/tidwall/geoindex"
	"github.com/tidwall/geojson"
	"github.com/tidwall/geojson/geo"
	"github.com/tidwall/geojson/geometry"
	"github.com/tidwall/rbang"
	"github.com/tidwall/tile38/internal/deadline"
	"github.com/tidwall/tinybtree"
)

// yieldStep forces the iterator to yield goroutine every 255 steps.
const yieldStep = 255

// Cursor allows for quickly paging through Scan, Within, Intersects, and Nearby
type Cursor interface {
	Offset() uint64
	Step(count uint64)
}

type itemT struct {
	id              string
	obj             geojson.Object
	fieldValuesSlot fieldValuesSlot
}

func (item *itemT) Less(other btree.Item, ctx interface{}) bool {
	value1 := item.obj.String()
	value2 := other.(*itemT).obj.String()
	if value1 < value2 {
		return true
	}
	if value1 > value2 {
		return false
	}
	// the values match so we'll compare IDs, which are always unique.
	return item.id < other.(*itemT).id
}

// Collection represents a collection of geojson objects.
type Collection struct {
	items       tinybtree.BTree // items sorted by keys
	index       *geoindex.Index // items geospatially indexed
	values      *btree.BTree    // items sorted by value+key
	fieldMap    map[string]int
	fieldArr    []string
	fieldValues *fieldValues
	weight      int
	points      int
	objects     int // geometry count
	nobjects    int // non-geometry count
}

var counter uint64

// New creates an empty collection
func New() *Collection {
	col := &Collection{
		index:       geoindex.Wrap(&rbang.RTree{}),
		values:      btree.New(32, nil),
		fieldMap:    make(map[string]int),
		fieldArr:    make([]string, 0),
		fieldValues: &fieldValues{},
	}
	return col
}

// Count returns the number of objects in collection.
func (c *Collection) Count() int {
	return c.objects + c.nobjects
}

// StringCount returns the number of string values.
func (c *Collection) StringCount() int {
	return c.nobjects
}

// PointCount returns the number of points (lat/lon coordinates) in collection.
func (c *Collection) PointCount() int {
	return c.points
}

// TotalWeight calculates the in-memory cost of the collection in bytes.
func (c *Collection) TotalWeight() int {
	return c.weight
}

// Bounds returns the bounds of all the items in the collection.
func (c *Collection) Bounds() (minX, minY, maxX, maxY float64) {
	min, max := c.index.Bounds()
	if len(min) >= 2 && len(max) >= 2 {
		return min[0], min[1], max[0], max[1]
	}
	return
}

func objIsSpatial(obj geojson.Object) bool {
	_, ok := obj.(geojson.Spatial)
	return ok
}

func (c *Collection) objWeight(item *itemT) int {
	var weight int
	if objIsSpatial(item.obj) {
		weight = item.obj.NumPoints() * 16
	} else {
		weight = len(item.obj.String())
	}
	return weight + len(c.fieldValues.get(item.fieldValuesSlot))*8 + len(item.id)
}

func (c *Collection) indexDelete(item *itemT) {
	if !item.obj.Empty() {
		rect := item.obj.Rect()
		c.index.Delete(
			[2]float64{rect.Min.X, rect.Min.Y},
			[2]float64{rect.Max.X, rect.Max.Y},
			item)
	}
}

func (c *Collection) indexInsert(item *itemT) {
	if !item.obj.Empty() {
		rect := item.obj.Rect()
		c.index.Insert(
			[2]float64{rect.Min.X, rect.Min.Y},
			[2]float64{rect.Max.X, rect.Max.Y},
			item)
	}
}

// Set adds or replaces an object in the collection and returns the fields
// array. If an item with the same id is already in the collection then the
// new item will adopt the old item's fields.
// The fields argument is optional.
// The return values are the old object, the old fields, and the new fields
func (c *Collection) Set(
	id string, obj geojson.Object, fields []string, values []float64,
) (
	oldObject geojson.Object, oldFieldValues []float64, newFieldValues []float64,
) {
	newItem := &itemT{id: id, obj: obj, fieldValuesSlot: nilValuesSlot}

	// add the new item to main btree and remove the old one if needed
	oldItem, ok := c.items.Set(id, newItem)
	if ok {
		oldItem := oldItem.(*itemT)
		// the old item was removed, now let's remove it from the rtree/btree.
		if objIsSpatial(oldItem.obj) {
			c.indexDelete(oldItem)
			c.objects--
		} else {
			c.values.Delete(oldItem)
			c.nobjects--
		}

		// decrement the point count
		c.points -= oldItem.obj.NumPoints()

		// decrement the weights
		c.weight -= c.objWeight(oldItem)

		// references
		oldObject = oldItem.obj
		oldFieldValues = c.fieldValues.get(oldItem.fieldValuesSlot)
		newFieldValues = oldFieldValues
		newItem.fieldValuesSlot = oldItem.fieldValuesSlot
	}

	if fields == nil {
		if len(values) > 0 {
			newFieldValues = values
			newFieldValuesSlot := c.fieldValues.set(newItem.fieldValuesSlot, newFieldValues)
			newItem.fieldValuesSlot = newFieldValuesSlot
		}
	} else {
		newFieldValues, _, _ = c.setFieldValues(newItem, fields, values)
	}

	// insert the new item into the rtree or strings tree.
	if objIsSpatial(newItem.obj) {
		c.indexInsert(newItem)
		c.objects++
	} else {
		c.values.ReplaceOrInsert(newItem)
		c.nobjects++
	}

	// increment the point count
	c.points += newItem.obj.NumPoints()

	// add the new weights
	c.weight += c.objWeight(newItem)

	return oldObject, oldFieldValues, newFieldValues
}

// Delete removes an object and returns it.
// If the object does not exist then the 'ok' return value will be false.
func (c *Collection) Delete(id string) (
	obj geojson.Object, fields []float64, ok bool,
) {
	oldItemV, ok := c.items.Delete(id)
	if !ok {
		return nil, nil, false
	}
	oldItem := oldItemV.(*itemT)
	if objIsSpatial(oldItem.obj) {
		if !oldItem.obj.Empty() {
			c.indexDelete(oldItem)
		}
		c.objects--
	} else {
		c.values.Delete(oldItem)
		c.nobjects--
	}
	c.weight -= c.objWeight(oldItem)
	c.points -= oldItem.obj.NumPoints()

	fields = c.fieldValues.get(oldItem.fieldValuesSlot)
	c.fieldValues.remove(oldItem.fieldValuesSlot)
	return oldItem.obj, fields, true
}

// Get returns an object.
// If the object does not exist then the 'ok' return value will be false.
func (c *Collection) Get(id string) (
	obj geojson.Object, fields []float64, ok bool,
) {
	itemV, ok := c.items.Get(id)
	if !ok {
		return nil, nil, false
	}
	item := itemV.(*itemT)
	return item.obj, c.fieldValues.get(item.fieldValuesSlot), true
}

// SetField set a field value for an object and returns that object.
// If the object does not exist then the 'ok' return value will be false.
func (c *Collection) SetField(id, field string, value float64) (
	obj geojson.Object, fields []float64, updated bool, ok bool,
) {
	itemV, ok := c.items.Get(id)
	if !ok {
		return nil, nil, false, false
	}
	item := itemV.(*itemT)
	_, updateCount, weightDelta := c.setFieldValues(item, []string{field}, []float64{value})
	c.weight += weightDelta
	return item.obj, c.fieldValues.get(item.fieldValuesSlot), updateCount > 0, true
}

// SetFields is similar to SetField, just setting multiple fields at once
func (c *Collection) SetFields(
	id string, inFields []string, inValues []float64,
) (obj geojson.Object, fields []float64, updatedCount int, ok bool) {
	itemV, ok := c.items.Get(id)
	if !ok {
		return nil, nil, 0, false
	}
	item := itemV.(*itemT)
	newFieldValues, updateCount, weightDelta := c.setFieldValues(item, inFields, inValues)
	c.weight += weightDelta
	return item.obj, newFieldValues, updateCount, true
}

func (c *Collection) setFieldValues(item *itemT, fields []string, updateValues []float64) (
	newValues []float64,
	updated int,
	weightDelta int,
) {
	newValues = c.fieldValues.get(item.fieldValuesSlot)
	for i, field := range fields {
		fieldIdx, ok := c.fieldMap[field]
		if !ok {
			fieldIdx = len(c.fieldMap)
			c.fieldMap[field] = fieldIdx
			c.addToFieldArr(field)
		}
		for fieldIdx >= len(newValues) {
			newValues = append(newValues, 0)
			weightDelta += 8
		}
		ovalue := newValues[fieldIdx]
		nvalue := updateValues[i]
		newValues[fieldIdx] = nvalue
		if ovalue != nvalue {
			updated++
		}
	}
	newSlot := c.fieldValues.set(item.fieldValuesSlot, newValues)
	item.fieldValuesSlot = newSlot
	return newValues, updated, weightDelta
}

// FieldMap return a maps of the field names.
func (c *Collection) FieldMap() map[string]int {
	return c.fieldMap
}

// FieldArr return an array representation of the field names.
func (c *Collection) FieldArr() []string {
	return c.fieldArr
}

// bsearch searches array for value.
func bsearch(arr []string, val string) (index int, found bool) {
	i, j := 0, len(arr)
	for i < j {
		h := i + (j-i)/2
		if val >= arr[h] {
			i = h + 1
		} else {
			j = h
		}
	}
	if i > 0 && arr[i-1] >= val {
		return i - 1, true
	}
	return i, false
}

func (c *Collection) addToFieldArr(field string) {
	if index, found := bsearch(c.fieldArr, field); !found {
		c.fieldArr = append(c.fieldArr, "")
		copy(c.fieldArr[index+1:], c.fieldArr[index:len(c.fieldArr)-1])
		c.fieldArr[index] = field
	}
}

// Scan iterates though the collection ids.
func (c *Collection) Scan(
	desc bool,
	cursor Cursor,
	deadline *deadline.Deadline,
	iterator func(id string, obj geojson.Object, fields []float64) bool,
) bool {
	var keepon = true
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	iter := func(key string, value interface{}) bool {
		count++
		if count <= offset {
			return true
		}
		nextStep(count, cursor, deadline)
		iitm := value.(*itemT)
		keepon = iterator(iitm.id, iitm.obj, c.fieldValues.get(iitm.fieldValuesSlot))
		return keepon
	}
	if desc {
		c.items.Reverse(iter)
	} else {
		c.items.Scan(iter)
	}
	return keepon
}

// ScanRange iterates though the collection starting with specified id.
func (c *Collection) ScanRange(
	start, end string,
	desc bool,
	cursor Cursor,
	deadline *deadline.Deadline,
	iterator func(id string, obj geojson.Object, fields []float64) bool,
) bool {
	var keepon = true
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	iter := func(key string, value interface{}) bool {
		count++
		if count <= offset {
			return true
		}
		nextStep(count, cursor, deadline)
		if !desc {
			if key >= end {
				return false
			}
		} else {
			if key <= end {
				return false
			}
		}
		iitm := value.(*itemT)
		keepon = iterator(iitm.id, iitm.obj, c.fieldValues.get(iitm.fieldValuesSlot))
		return keepon
	}

	if desc {
		c.items.Descend(start, iter)
	} else {
		c.items.Ascend(start, iter)
	}
	return keepon
}

// SearchValues iterates though the collection values.
func (c *Collection) SearchValues(
	desc bool,
	cursor Cursor,
	deadline *deadline.Deadline,
	iterator func(id string, obj geojson.Object, fields []float64) bool,
) bool {
	var keepon = true
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	iter := func(item btree.Item) bool {
		count++
		if count <= offset {
			return true
		}
		nextStep(count, cursor, deadline)
		iitm := item.(*itemT)
		keepon = iterator(iitm.id, iitm.obj, c.fieldValues.get(iitm.fieldValuesSlot))
		return keepon
	}
	if desc {
		c.values.Descend(iter)
	} else {
		c.values.Ascend(iter)
	}
	return keepon
}

// SearchValuesRange iterates though the collection values.
func (c *Collection) SearchValuesRange(start, end string, desc bool,
	cursor Cursor,
	deadline *deadline.Deadline,
	iterator func(id string, obj geojson.Object, fields []float64) bool,
) bool {
	var keepon = true
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	iter := func(item btree.Item) bool {
		count++
		if count <= offset {
			return true
		}
		nextStep(count, cursor, deadline)
		iitm := item.(*itemT)
		keepon = iterator(iitm.id, iitm.obj, c.fieldValues.get(iitm.fieldValuesSlot))
		return keepon
	}
	if desc {
		c.values.DescendRange(&itemT{obj: String(start)},
			&itemT{obj: String(end)}, iter)
	} else {
		c.values.AscendRange(&itemT{obj: String(start)},
			&itemT{obj: String(end)}, iter)
	}
	return keepon
}

// ScanGreaterOrEqual iterates though the collection starting with specified id.
func (c *Collection) ScanGreaterOrEqual(id string, desc bool,
	cursor Cursor,
	deadline *deadline.Deadline,
	iterator func(id string, obj geojson.Object, fields []float64) bool,
) bool {
	var keepon = true
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	iter := func(key string, value interface{}) bool {
		count++
		if count <= offset {
			return true
		}
		nextStep(count, cursor, deadline)
		iitm := value.(*itemT)
		keepon = iterator(iitm.id, iitm.obj, c.fieldValues.get(iitm.fieldValuesSlot))
		return keepon
	}
	if desc {
		c.items.Descend(id, iter)
	} else {
		c.items.Ascend(id, iter)
	}
	return keepon
}

func (c *Collection) geoSearch(
	rect geometry.Rect,
	iter func(id string, obj geojson.Object, fields []float64) bool,
) bool {
	alive := true
	c.index.Search(
		[2]float64{rect.Min.X, rect.Min.Y},
		[2]float64{rect.Max.X, rect.Max.Y},
		func(_, _ [2]float64, itemv interface{}) bool {
			item := itemv.(*itemT)
			alive = iter(item.id, item.obj, c.fieldValues.get(item.fieldValuesSlot))
			return alive
		},
	)
	return alive
}

func (c *Collection) geoSparse(
	obj geojson.Object, sparse uint8,
	iter func(id string, obj geojson.Object, fields []float64) (match, ok bool),
) bool {
	matches := make(map[string]bool)
	alive := true
	c.geoSparseInner(obj.Rect(), sparse,
		func(id string, o geojson.Object, fields []float64) (
			match, ok bool,
		) {
			ok = true
			if !matches[id] {
				match, ok = iter(id, o, fields)
				if match {
					matches[id] = true
				}
			}
			return match, ok
		},
	)
	return alive
}
func (c *Collection) geoSparseInner(
	rect geometry.Rect, sparse uint8,
	iter func(id string, obj geojson.Object, fields []float64) (match, ok bool),
) bool {
	if sparse > 0 {
		w := rect.Max.X - rect.Min.X
		h := rect.Max.Y - rect.Min.Y
		quads := [4]geometry.Rect{
			geometry.Rect{
				Min: geometry.Point{X: rect.Min.X, Y: rect.Min.Y + h/2},
				Max: geometry.Point{X: rect.Min.X + w/2, Y: rect.Max.Y},
			},
			geometry.Rect{
				Min: geometry.Point{X: rect.Min.X + w/2, Y: rect.Min.Y + h/2},
				Max: geometry.Point{X: rect.Max.X, Y: rect.Max.Y},
			},
			geometry.Rect{
				Min: geometry.Point{X: rect.Min.X, Y: rect.Min.Y},
				Max: geometry.Point{X: rect.Min.X + w/2, Y: rect.Min.Y + h/2},
			},
			geometry.Rect{
				Min: geometry.Point{X: rect.Min.X + w/2, Y: rect.Min.Y},
				Max: geometry.Point{X: rect.Max.X, Y: rect.Min.Y + h/2},
			},
		}
		for _, quad := range quads {
			if !c.geoSparseInner(quad, sparse-1, iter) {
				return false
			}
		}
		return true
	}
	alive := true
	c.geoSearch(rect,
		func(id string, obj geojson.Object, fields []float64) bool {
			match, ok := iter(id, obj, fields)
			if !ok {
				alive = false
				return false
			}
			return !match
		},
	)
	return alive
}

// Within returns all object that are fully contained within an object or
// bounding box. Set obj to nil in order to use the bounding box.
func (c *Collection) Within(
	obj geojson.Object,
	sparse uint8,
	cursor Cursor,
	deadline *deadline.Deadline,
	iter func(id string, obj geojson.Object, fields []float64) bool,
) bool {
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	if sparse > 0 {
		return c.geoSparse(obj, sparse,
			func(id string, o geojson.Object, fields []float64) (
				match, ok bool,
			) {
				count++
				if count <= offset {
					return false, true
				}
				nextStep(count, cursor, deadline)
				if match = o.Within(obj); match {
					ok = iter(id, o, fields)
				}
				return match, ok
			},
		)
	}
	return c.geoSearch(obj.Rect(),
		func(id string, o geojson.Object, fields []float64) bool {
			count++
			if count <= offset {
				return true
			}
			nextStep(count, cursor, deadline)
			if o.Within(obj) {
				return iter(id, o, fields)
			}
			return true
		},
	)
}

// Intersects returns all object that are intersect an object or bounding box.
// Set obj to nil in order to use the bounding box.
func (c *Collection) Intersects(
	obj geojson.Object,
	sparse uint8,
	cursor Cursor,
	deadline *deadline.Deadline,
	iter func(id string, obj geojson.Object, fields []float64) bool,
) bool {
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	if sparse > 0 {
		return c.geoSparse(obj, sparse,
			func(id string, o geojson.Object, fields []float64) (
				match, ok bool,
			) {
				count++
				if count <= offset {
					return false, true
				}
				nextStep(count, cursor, deadline)
				if match = o.Intersects(obj); match {
					ok = iter(id, o, fields)
				}
				return match, ok
			},
		)
	}
	return c.geoSearch(obj.Rect(),
		func(id string, o geojson.Object, fields []float64) bool {
			count++
			if count <= offset {
				return true
			}
			nextStep(count, cursor, deadline)
			if o.Intersects(obj) {
				return iter(id, o, fields)
			}
			return true
		},
	)
}

// Nearby returns the nearest neighbors
func (c *Collection) Nearby(
	target geojson.Object,
	cursor Cursor,
	deadline *deadline.Deadline,
	iter func(id string, obj geojson.Object, fields []float64) bool,
) bool {
	// First look to see if there's at least one candidate in the circle's
	// outer rectangle. This is a fast-fail operation.
	if circle, ok := target.(*geojson.Circle); ok {
		meters := circle.Meters()
		if meters > 0 {
			center := circle.Center()
			minLat, minLon, maxLat, maxLon :=
				geo.RectFromCenter(center.Y, center.X, meters)
			var exists bool
			c.index.Search(
				[2]float64{minLon, minLat},
				[2]float64{maxLon, maxLat},
				func(_, _ [2]float64, itemv interface{}) bool {
					exists = true
					return false
				},
			)
			if !exists {
				// no candidates
				return true
			}
		}
	}
	// do the kNN operation
	alive := true
	center := target.Center()
	var count uint64
	var offset uint64
	if cursor != nil {
		offset = cursor.Offset()
		cursor.Step(offset)
	}
	c.index.Nearby(
		geoindex.SimpleBoxAlgo(
			[2]float64{center.X, center.Y},
			[2]float64{center.X, center.Y},
		),
		func(_, _ [2]float64, itemv interface{}, _ float64) bool {
			count++
			if count <= offset {
				return true
			}
			nextStep(count, cursor, deadline)
			item := itemv.(*itemT)
			alive = iter(item.id, item.obj, c.fieldValues.get(item.fieldValuesSlot))
			return alive
		},
	)
	return alive
}

func nextStep(step uint64, cursor Cursor, deadline *deadline.Deadline) {
	if step&yieldStep == yieldStep {
		runtime.Gosched()
		deadline.Check()
	}
	if cursor != nil {
		cursor.Step(1)
	}
}
