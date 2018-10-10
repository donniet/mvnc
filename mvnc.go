package mvnc

// #include <stdio.h>
// #include <stdlib.h>
// #cgo LDFLAGS: -lmvnc
// #include <mvnc.h>
import "C"

import (
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"io/ioutil"
	"log"
	"math"
	"os"
	"time"
	"unsafe"
)

type Graph struct {
	GraphFile string
	Names     map[int]string
	Threshold float32
	Throttle  time.Duration
}

func (f Graph) Process(reader io.Reader) <-chan string {
	r := make(chan string)

	go f.thread(reader, r)

	return r
}

func errorFor(status C.ncStatus_t) error {
	switch status {
	case C.NC_OK:
		return nil
	case C.NC_BUSY:
		return fmt.Errorf("NC_BUSY: The device is busy; retry later.")
	case C.NC_ERROR:
		return fmt.Errorf("NC_ERROR: An unexpected error was encountered during the function call.")
	case C.NC_OUT_OF_MEMORY:
		return fmt.Errorf("NC_OUT_OF_MEMORY: The host is out of memory.")
	case C.NC_DEVICE_NOT_FOUND:
		return fmt.Errorf("NC_DEVICE_NOT_FOUND: There is no device at the given index or name.")
	case C.NC_INVALID_PARAMETERS:
		return fmt.Errorf("NC_INVALID_PARAMETERS: At least one of the given parameters is invalid in the context of the function call.")
	case C.NC_TIMEOUT:
		return fmt.Errorf("NC_TIMEOUT: Timeout in the communication with the device.")
	case C.NC_MVCMD_NOT_FOUND:
		return fmt.Errorf("NC_MVCMD_NOT_FOUND: The file to boot the device was not found. This file typically has the extension .mvcmd and should be installed during the NCSDK installation. This message may mean that the installation failed.")
	case C.NC_NOT_ALLOCATED:
		return fmt.Errorf("NC_NOT_ALLOCATED: The graph or fifo has not been allocated.")
	case C.NC_UNAUTHORIZED:
		return fmt.Errorf("NC_UNAUTHORIZED: An unauthorized operation has been attempted.")
	case C.NC_UNSUPPORTED_GRAPH_FILE:
		return fmt.Errorf("NC_UNSUPPORTED_GRAPH_FILE: The graph file may have been created with an incompatible prior version of the Toolkit. Try to recompile the graph file with the version of the Toolkit that corresponds to the API version.")
	case C.NC_UNSUPPORTED_CONFIGURATION_FILE:
		return fmt.Errorf("NC_UNSUPPORTED_CONFIGURATION_FILE: Unsupported configuration file")
	case C.NC_UNSUPPORTED_FEATURE:
		return fmt.Errorf("NC_UNSUPPORTED_FEATURE: Operation attempted a feature that is not supported by this firmware version.")
	case C.NC_MYRIAD_ERROR:
		return fmt.Errorf("NC_MYRIAD_ERROR: An error has been reported by Intel® Movidius™ VPU. Use ncGraphGetOption() for NC_RO_GRAPH_DEBUG_INFO and ncDeviceGetOption for NC_RO_DEVICE_DEBUG_INFO to get more information on the error.")
	case C.NC_INVALID_DATA_LENGTH:
		return fmt.Errorf("NC_INVALID_DATA_LENGTH: An invalid data length has been passed when getting or setting an option.")
	case C.NC_INVALID_HANDLE:
		return fmt.Errorf("NC_INVALID_HANDLE: An invalid handle has been passed to a function.")
	default:
		return fmt.Errorf("unknown MVNC error: '%v'", status)
	}
}

type RawRGBImage struct {
	bytes  []byte
	width  int
	height int
}

func (r *RawRGBImage) ColorModel() color.Model {
	return color.RGBAModel
}
func (r *RawRGBImage) Bounds() image.Rectangle {
	return image.Rectangle{
		Min: image.Point{0, 0},
		Max: image.Point{r.width, r.height},
	}
}
func (r *RawRGBImage) At(x, y int) color.Color {
	pos := (x + y*r.width) * 3

	return color.RGBA{
		r.bytes[pos],
		r.bytes[pos+1],
		r.bytes[pos+2],
		1.0,
	}
}

func (f Graph) thread(reader io.Reader, detected chan<- string) {
	last := time.Now()

	defer close(detected)

	var deviceHandle *C.struct_ncDeviceHandle_t
	var graphHandle *C.struct_ncGraphHandle_t

	if ret := C.ncDeviceCreate(0, &deviceHandle); ret != C.NC_OK {
		log.Printf("could not get device name,  %v", errorFor(ret))
		return
	}
	defer C.ncDeviceDestroy(&deviceHandle)

	if ret := C.ncDeviceOpen(deviceHandle); ret != C.NC_OK {
		log.Printf("could not open device: %v", errorFor(ret))
		return
	}
	defer C.ncDeviceClose(deviceHandle)

	if ret := C.ncGraphCreate(C.CString("faces"), &graphHandle); ret != C.NC_OK {
		log.Printf("could not create graph, %v", errorFor(ret))
		return
	}
	defer C.ncGraphDestroy(&graphHandle)

	var inputFifo, outputFifo *C.struct_ncFifoHandle_t

	if b, err := ioutil.ReadFile(f.GraphFile); err != nil {
		log.Println(err)
		return
	} else if ret := C.ncGraphAllocateWithFifos(deviceHandle, graphHandle, unsafe.Pointer(&b[0]), C.uint(len(b)), &inputFifo, &outputFifo); ret != C.NC_OK {
		log.Printf("error allocating graph: %v", errorFor(ret))
		return
	}

	defer C.ncFifoDestroy(&inputFifo)
	defer C.ncFifoDestroy(&outputFifo)

	fifoOutputSize := C.uint(0)
	fifoInputSize := C.uint(0)
	optionDataLen := C.uint(4)

	C.ncFifoGetOption(outputFifo, C.NC_RO_FIFO_ELEMENT_DATA_SIZE, unsafe.Pointer(&fifoOutputSize), &optionDataLen)
	C.ncFifoGetOption(inputFifo, C.NC_RO_FIFO_ELEMENT_DATA_SIZE, unsafe.Pointer(&fifoInputSize), &optionDataLen)

	log.Printf("fifo input/output sizes: %d/%d", fifoInputSize, fifoOutputSize)
	// data expected by the fifo is floats (4 bytes per channel), but the image is read in as 1 byte per channel
	readerInputSize := fifoInputSize / 4

	bb := make([]byte, readerInputSize)
	input := make([]float32, readerInputSize)

	log.Printf("reader input size: %d", readerInputSize)

	if int(fifoOutputSize)/4 > len(f.Names) {
		log.Printf("outputsize %d greater than names %d", fifoOutputSize/4, len(f.Names))
	}

	bout := make([]float32, fifoOutputSize/4)

	for {
		// cur := 0
		// for {
		// 	if n, err := reader.Read(bb[cur:]); err != nil {
		// 		log.Println(err)
		// 		return
		// 	} else if cur+n == len(bb) {
		// 		break
		// 	} else {
		// 		cur += n
		// 	}
		// }
		if n, err := reader.Read(bb); err != nil {
			log.Println(err)
			return
		} else if n < len(bb) {
			log.Println("not enough data read: %d expected %d", n, len(bb))
			return
		}

		size := int(math.Sqrt(float64(len(bb) / 3)))
		img := &RawRGBImage{
			bytes:  bb,
			width:  size,
			height: size,
		}
		out, _ := os.OpenFile("test.jpg", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		jpeg.Encode(out, img, &jpeg.Options{75})
		out.Close()

		fifoWriteFillLevel := C.int(0)
		fifoWriteFillLevelSize := C.uint(4)

		if now := time.Now(); now.Sub(last) < f.Throttle {
			log.Printf("throttling")
			continue
		} else if ret := C.ncFifoGetOption(inputFifo, C.NC_RO_FIFO_WRITE_FILL_LEVEL, unsafe.Pointer(&fifoWriteFillLevel), &fifoWriteFillLevelSize); ret != C.NC_OK {
			log.Printf("error getting fifo fill level %v", errorFor(ret))
			return
		} else if fifoWriteFillLevel > 0 {
			log.Println("fifo has elements, skipping this frame")
			continue
		} else {
			last = now
		}

		// convert bytes read in into floats for the movidius-- I wish we could do this on the device...
		for i, c := range bb {
			input[i] = (float32(c) - 128.0) / 256.0
		}

		user := unsafe.Pointer(nil)

		if ret := C.ncFifoWriteElem(inputFifo, unsafe.Pointer(&input[0]), &fifoInputSize, unsafe.Pointer(nil)); ret != C.NC_OK {
			log.Printf("error writing fifo, %v", errorFor(ret))
			return
		} else if ret := C.ncGraphQueueInference(graphHandle, &inputFifo, 1, &outputFifo, 1); ret != C.NC_OK {
			log.Printf("error queuing inference, %v", errorFor(ret))
			return
		} else if ret := C.ncFifoReadElem(outputFifo, unsafe.Pointer(&bout[0]), &fifoOutputSize, &user); ret != C.NC_OK {
			log.Printf("error reading output of inference, %v", errorFor(ret))
			return
		}

		log.Printf("mvnc: %v", bout)

		for i, r := range bout {
			if n, ok := f.Names[i]; ok && r > f.Threshold {
				detected <- n
			}
		}
	}
}
