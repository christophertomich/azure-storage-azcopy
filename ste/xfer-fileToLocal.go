// Copyright © 2017 Microsoft <wastore@microsoft.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package ste

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"

	"net/http"

	"github.com/Azure/azure-pipeline-go/pipeline"
	"github.com/Azure/azure-storage-azcopy/common"
	"github.com/Azure/azure-storage-file-go/2017-07-29/azfile"
)

// TODO: Unify blobToLocal and fileToLocal after code review and logic is finalized.
func FileToLocal(jptm IJobPartTransferMgr, p pipeline.Pipeline, pacer *pacer) {

	info := jptm.Info()
	u, _ := url.Parse(info.Source)

	srcFileURL := azfile.NewFileURL(*u, p)
	// step 2: get size info for the download
	fileSize := int64(info.SourceSize)
	downloadChunkSize := int64(info.BlockSize)
	numChunks := uint32(0)

	if jptm.ShouldLog(pipeline.LogInfo) {
		jptm.LogTransferStart(info.Source, info.Destination, fmt.Sprintf("Chunk size %d", downloadChunkSize))
	}

	// If the transfer was cancelled, then reporting transfer as done and increasing the bytestransferred by the size of the source.
	if jptm.WasCanceled() {
		jptm.ReportTransferDone()
		return
	}

	// If the force Write flags is set to false
	// then check the file exists locally or not.
	// If it does, mark transfer as failed.
	if !jptm.IsForceWriteTrue() {
		_, err := os.Stat(info.Destination)
		if err == nil {
			// If the error is nil, then blob exists locally and it doesn't needs to be downloaded.
			jptm.LogDownloadError(info.Source, info.Destination, "File already exists ", 0)
			// Mark the transfer as failed with FileAlreadyExistsFailure
			jptm.SetStatus(common.ETransferStatus.FileAlreadyExistsFailure())
			jptm.ReportTransferDone()
			return
		}
	}

	// step 3: prep local file before download starts
	if fileSize == 0 {
		err := createEmptyFile(info.Destination)
		if err != nil {
			// If the error is nil, then blob exists locally and it doesn't needs to be downloaded.
			jptm.LogDownloadError(info.Source, info.Destination, "Empty File Creation Error "+err.Error(), 0)
			jptm.SetStatus(common.ETransferStatus.Failed())
			jptm.ReportTransferDone()
			return
		}
		lMTime, plmt := jptm.PreserveLastModifiedTime()
		if plmt {
			err := os.Chtimes(jptm.Info().Destination, lMTime, lMTime)
			if err != nil {
				// If the error is nil, then blob exists locally and it doesn't needs to be downloaded.
				jptm.LogDownloadError(info.Source, info.Destination, "Preserve Modified Time Error "+err.Error(), 0)
				//delete the file if transfer failed
				err := os.Remove(info.Destination)
				if err != nil {
					jptm.LogError(info.Destination, "Delete File Error ", err)
				}
				jptm.SetStatus(common.ETransferStatus.Failed())
				return
			}
			if jptm.ShouldLog(pipeline.LogInfo) {
				jptm.Log(pipeline.LogInfo, fmt.Sprintf(" Preserved the last modified time for destination %s", info.Destination))
			}
		}

		// executing the epilogue.
		jptm.Log(pipeline.LogInfo, "DOWNLOAD SUCCESSFUL")
		jptm.SetStatus(common.ETransferStatus.Success())
		jptm.ReportTransferDone()

	} else { // 3b: source has content
		dstFile, err := common.CreateFileOfSize(info.Destination, fileSize)
		if err != nil {
			// If the error is nil, then blob exists locally and it doesn't needs to be downloaded.
			jptm.LogDownloadError(info.Source, info.Destination, "File Creation Error "+err.Error(), 0)
			jptm.SetStatus(common.ETransferStatus.Failed())

			//delete the file if transfer failed
			err := os.Remove(info.Destination)
			if err != nil {
				jptm.LogError(info.Destination, "Delete File Error ", err)
			}
			jptm.ReportTransferDone()
			return
		}

		if rem := fileSize % downloadChunkSize; rem == 0 {
			numChunks = uint32(fileSize / downloadChunkSize)
		} else {
			numChunks = uint32(fileSize/downloadChunkSize + 1)
		}
		jptm.SetNumberOfChunks(numChunks)
		chunkIDCount := int32(0)
		// step 4: go through the file range and schedule download chunk jobs
		for startIndex := int64(0); startIndex < fileSize; startIndex += downloadChunkSize {
			adjustedChunkSize := downloadChunkSize

			// compute exact size of the chunk
			if startIndex+downloadChunkSize > fileSize {
				adjustedChunkSize = fileSize - startIndex
			}

			// schedule the download chunk job
			jptm.ScheduleChunks(generateDownloadFileFunc(jptm, srcFileURL, dstFile, startIndex, adjustedChunkSize))
			chunkIDCount++
		}
	}
}

func generateDownloadFileFunc(jptm IJobPartTransferMgr, transferFileURL azfile.FileURL, dstFile *os.File, startIndex int64, adjustedChunkSize int64) chunkFunc {
	return func(workerId int) {
		info := jptm.Info()
		chunkDone := func() {
			// adding the bytes transferred or skipped of a transfer to determine the progress of transfer.
			lastChunk, _ := jptm.ReportChunkDone()
			if lastChunk {
				if jptm.ShouldLog(pipeline.LogDebug) {
					jptm.Log(pipeline.LogDebug, "Finalizing transfer cancellation")
				}
				dstFile.Close()
				// If the status of transfer is less than or equal to 0
				// then transfer failed or cancelled
				// the downloaded file needs to be deleted
				if jptm.TransferStatus() <= 0 {
					err := os.Remove(info.Destination)
					if err != nil {
						jptm.LogError(info.Destination, "Delete File Error ", err)
					}
				}
				jptm.ReportTransferDone()
			}
		}
		if jptm.WasCanceled() {
			chunkDone()
		} else {
			dstMMF, err := common.NewMMF(dstFile, true, startIndex, adjustedChunkSize)
			if err != nil {
				if !jptm.WasCanceled() {
					jptm.Cancel()
					status, msg := ErrorEx{err}.ErrorCodeAndString()
					jptm.LogDownloadError(info.Source, info.Destination, msg, status)
					jptm.SetStatus(common.ETransferStatus.Failed())
					jptm.SetErrorCode(int32(status))
				}
				chunkDone()
				return
			}
			defer dstMMF.Unmap()

			// step 1: Downloading the file from range startIndex till (startIndex + adjustedChunkSize)
			get, err := transferFileURL.Download(jptm.Context(), startIndex, adjustedChunkSize, false)
			if err != nil {
				if !jptm.WasCanceled() {
					jptm.Cancel()
					status, msg := ErrorEx{err}.ErrorCodeAndString()
					jptm.LogDownloadError(info.Source, info.Destination, msg, status)
					jptm.SetStatus(common.ETransferStatus.Failed())
					jptm.SetErrorCode(int32(status))
					// If the status code was 403, it means there was an authentication error and we exit.
					// User can resume the job if completely ordered with a new sas.
					if status == http.StatusForbidden {
						common.GetLifecycleMgr().Exit(fmt.Sprintf("Authentication Failed. The SAS is not correct or expired or does not have the correct permission %s", err.Error()), 1)
					}
				}
				chunkDone()
				return
			}

			// step 2: write the body into the memory mapped file directly
			retryReader := get.Body(azfile.RetryReaderOptions{MaxRetryRequests: MaxRetryPerDownloadBody})
			_, err = io.ReadFull(retryReader, dstMMF.Slice())
			io.Copy(ioutil.Discard, retryReader)
			retryReader.Close()
			if err != nil {
				// cancel entire transfer because this chunk has failed
				if !jptm.WasCanceled() {
					jptm.Cancel()
					status, msg := ErrorEx{err}.ErrorCodeAndString()
					jptm.LogDownloadError(info.Source, info.Destination, msg, status)
					jptm.SetStatus(common.ETransferStatus.Failed())
					jptm.SetErrorCode(int32(status))
					// If the status code was 403, it means there was an authentication error and we exit.
					// User can resume the job if completely ordered with a new sas.
					if status == http.StatusForbidden {
						common.GetLifecycleMgr().Exit(fmt.Sprintf("Authentication Failed. The SAS is not correct or expired or does not have the correct permission %s", err.Error()), 1)
					}
				}
				chunkDone()
				return
			}

			lastChunk, _ := jptm.ReportChunkDone()
			// step 3: check if this is the last chunk
			if lastChunk {
				// step 4: this is the last block, perform EPILOGUE
				if jptm.ShouldLog(pipeline.LogDebug) {
					jptm.Log(pipeline.LogDebug, "DOWNLOAD SUCCESSFUL")
				}
				jptm.SetStatus(common.ETransferStatus.Success())
				if jptm.ShouldLog(pipeline.LogDebug) {
					jptm.Log(pipeline.LogDebug, "Finalizing Transfer")
				}
				jptm.ReportTransferDone()

				dstFile.Close()

				lastModifiedTime, preserveLastModifiedTime := jptm.PreserveLastModifiedTime()
				if preserveLastModifiedTime {
					err := os.Chtimes(jptm.Info().Destination, lastModifiedTime, lastModifiedTime)
					if err != nil {
						jptm.LogError(info.Destination, "Preserved Modified Time Error ", err)
						return
					}
					if jptm.ShouldLog(pipeline.LogInfo) {
						jptm.Log(pipeline.LogInfo, fmt.Sprintf("Preserved modified time for %s", info.Destination))
					}
				}
			}
		}
	}
}
