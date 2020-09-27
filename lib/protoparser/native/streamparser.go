package native

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sync"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/encoding"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/protoparser/common"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"
	"github.com/VictoriaMetrics/metrics"
)

// ParseStream parses /api/v1/import/native lines from req and calls callback for parsed blocks.
//
// The callback can be called multiple times for streamed data from req.
//
// callback shouldn't hold block after returning.
// callback can be called in parallel from multiple concurrent goroutines.
func ParseStream(req *http.Request, callback func(block *Block) error) error {
	r := req.Body
	if req.Header.Get("Content-Encoding") == "gzip" {
		zr, err := common.GetGzipReader(r)
		if err != nil {
			return fmt.Errorf("cannot read gzipped vmimport data: %w", err)
		}
		defer common.PutGzipReader(zr)
		r = zr
	}
	// By default req.Body uses 4Kb buffer. This size is too small for typical request to /api/v1/import/native,
	// so use slightly bigger buffer in order to reduce read syscall overhead.
	br := bufio.NewReaderSize(r, 1024*1024)

	// Read time range (tr)
	trBuf := make([]byte, 16)
	var tr storage.TimeRange
	if _, err := io.ReadFull(br, trBuf); err != nil {
		readErrors.Inc()
		return fmt.Errorf("cannot read time range: %w", err)
	}
	tr.MinTimestamp = encoding.UnmarshalInt64(trBuf)
	tr.MaxTimestamp = encoding.UnmarshalInt64(trBuf[8:])

	// Start GOMAXPROC workers in order to process ingested data in parallel.
	gomaxprocs := runtime.GOMAXPROCS(-1)
	workCh := make(chan *unmarshalWork, 8*gomaxprocs)
	var wg sync.WaitGroup
	defer func() {
		close(workCh)
		wg.Wait()
	}()
	wg.Add(gomaxprocs)
	for i := 0; i < gomaxprocs; i++ {
		go func() {
			defer wg.Done()
			var tmpBlock storage.Block
			for uw := range workCh {
				if err := uw.unmarshal(&tmpBlock, tr); err != nil {
					parseErrors.Inc()
					logger.Errorf("error when unmarshaling native block: %s", err)
					putUnmarshalWork(uw)
					continue
				}
				if err := callback(&uw.block); err != nil {
					processErrors.Inc()
					logger.Errorf("error when processing native block: %s", err)
					putUnmarshalWork(uw)
					continue
				}
				putUnmarshalWork(uw)
			}
		}()
	}

	// Read native blocks and feed workers with work.
	sizeBuf := make([]byte, 4)
	for {
		uw := getUnmarshalWork()

		// Read uw.metricNameBuf
		if _, err := io.ReadFull(br, sizeBuf); err != nil {
			if err == io.EOF {
				// End of stream
				return nil
			}
			readErrors.Inc()
			return fmt.Errorf("cannot read metricName size: %w", err)
		}
		readCalls.Inc()
		bufSize := encoding.UnmarshalUint32(sizeBuf)
		if bufSize > 1024*1024 {
			parseErrors.Inc()
			return fmt.Errorf("too big metricName size; got %d; shouldn't exceed %d", bufSize, 1024*1024)
		}
		uw.metricNameBuf = bytesutil.Resize(uw.metricNameBuf, int(bufSize))
		if _, err := io.ReadFull(br, uw.metricNameBuf); err != nil {
			readErrors.Inc()
			return fmt.Errorf("cannot read metricName with size %d bytes: %w", bufSize, err)
		}
		readCalls.Inc()

		// Read uw.blockBuf
		if _, err := io.ReadFull(br, sizeBuf); err != nil {
			readErrors.Inc()
			return fmt.Errorf("cannot read native block size: %w", err)
		}
		readCalls.Inc()
		bufSize = encoding.UnmarshalUint32(sizeBuf)
		if bufSize > 1024*1024 {
			parseErrors.Inc()
			return fmt.Errorf("too big native block size; got %d; shouldn't exceed %d", bufSize, 1024*1024)
		}
		uw.blockBuf = bytesutil.Resize(uw.blockBuf, int(bufSize))
		if _, err := io.ReadFull(br, uw.blockBuf); err != nil {
			readErrors.Inc()
			return fmt.Errorf("cannot read native block with size %d bytes: %w", bufSize, err)
		}
		readCalls.Inc()
		blocksRead.Inc()

		// Feed workers with work.
		workCh <- uw
	}
}

// Block is a single block from `/api/v1/import/native` request.
type Block struct {
	MetricName storage.MetricName
	Values     []float64
	Timestamps []int64
}

func (b *Block) reset() {
	b.MetricName.Reset()
	b.Values = b.Values[:0]
	b.Timestamps = b.Timestamps[:0]
}

var (
	readCalls  = metrics.NewCounter(`vm_protoparser_read_calls_total{type="native"}`)
	readErrors = metrics.NewCounter(`vm_protoparser_read_errors_total{type="native"}`)
	rowsRead   = metrics.NewCounter(`vm_protoparser_rows_read_total{type="native"}`)
	blocksRead = metrics.NewCounter(`vm_protoparser_blocks_read_total{type="native"}`)

	parseErrors   = metrics.NewCounter(`vm_protoparser_parse_errors_total{type="native"}`)
	processErrors = metrics.NewCounter(`vm_protoparser_process_errors_total{type="native"}`)
)

type unmarshalWork struct {
	tr            storage.TimeRange
	metricNameBuf []byte
	blockBuf      []byte
	block         Block
}

func (uw *unmarshalWork) reset() {
	uw.metricNameBuf = uw.metricNameBuf[:0]
	uw.blockBuf = uw.blockBuf[:0]
	uw.block.reset()
}

func (uw *unmarshalWork) unmarshal(tmpBlock *storage.Block, tr storage.TimeRange) error {
	block := &uw.block
	if err := block.MetricName.UnmarshalNoAccountIDProjectID(uw.metricNameBuf); err != nil {
		return fmt.Errorf("cannot unmarshal metricName from %d bytes: %w", len(uw.metricNameBuf), err)
	}
	tail, err := tmpBlock.UnmarshalPortable(uw.blockBuf)
	if err != nil {
		return fmt.Errorf("cannot unmarshal native block from %d bytes: %w", len(uw.blockBuf), err)
	}
	if len(tail) > 0 {
		return fmt.Errorf("unexpected non-empty tail left after unmarshaling native block from %d bytes; len(tail)=%d bytes", len(uw.blockBuf), len(tail))
	}
	block.Timestamps, block.Values = tmpBlock.AppendRowsWithTimeRangeFilter(block.Timestamps[:0], block.Values[:0], tr)
	rowsRead.Add(len(block.Timestamps))
	return nil
}

func getUnmarshalWork() *unmarshalWork {
	v := unmarshalWorkPool.Get()
	if v == nil {
		return &unmarshalWork{}
	}
	return v.(*unmarshalWork)
}

func putUnmarshalWork(uw *unmarshalWork) {
	uw.reset()
	unmarshalWorkPool.Put(uw)
}

var unmarshalWorkPool sync.Pool