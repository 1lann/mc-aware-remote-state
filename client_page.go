package main

import (
	"context"
	"database/sql"
	"log"
	"strconv"
	"time"

	"github.com/1lann/mc-aware-remote-state/store"
	"github.com/cockroachdb/errors"
	"github.com/google/uuid"
)

func (c *SQLBackedClient) readPage(ctx context.Context, fileID uuid.UUID, pageNum uint32) ([]byte, error) {
	singleflightKey := fileID.String() + ":" + strconv.FormatInt(int64(pageNum), 10)

	// log.Printf("reading %q page %d", fileID, pageNum)

	result := c.fileGroup.DoChan(singleflightKey, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
		defer cancel()

		pageKey := PageKey{
			FileID:  fileID,
			PageNum: uint32(pageNum),
		}
		data, err := c.PagePool.GetPage(pageKey)
		if errors.Is(err, ErrNotFound) {
			// fetch from database
			data, err := c.Q.GetPage(ctx, store.GetPageParams{
				FileID:        fileID[:],
				PageNum:       int64(pageNum),
				PageSizePower: int64(c.pagePower),
			})
			log.Println("no page found in pool!!")
			if errors.Is(err, sql.ErrNoRows) {
				// create the page if it doesn't exist
				if err := c.PagePool.AddPage(pageKey, nil, true); err != nil {
					return nil, errors.Wrap(err, "AddPage for newly created page")
				}
				return ([]byte)(nil), nil
			} else if err != nil {
				return nil, errors.Wrap(err, "store.GetPage")
			}

			err = c.PagePool.AddPage(pageKey, data, false)
			if err != nil {
				log.Println("error adding page to pool:", fileID, pageNum)
				// skip handling error for now, the pool is merely a cache
			}

			return data, nil
		} else if errors.Is(err, ErrTombstoned) {
			// create the page if it doesn't exist
			if err := c.PagePool.AddPage(pageKey, nil, true); err != nil {
				return nil, errors.Wrap(err, "AddPage for newly created page")
			}
			return ([]byte)(nil), nil
		} else if err != nil {
			return nil, errors.Wrap(err, "PagePool.GetPage")
		}

		return data, nil
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-result:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.([]byte), nil
	}
}

func (c *SQLBackedClient) ReadFile(ctx context.Context, fileID uuid.UUID, offset int64, data []byte) (int, error) {
	if offset < 0 {
		return 0, errors.New("offset must be positive")
	}

	lock := c.getFileLock(fileID)
	lock.RLock()
	defer func() {
		lock.RUnlock()
		lock.Release()
	}()

	fileMeta, err := c.GetFile(ctx, fileID)
	if err != nil {
		return 0, err
	}

	if fileMeta.FileType != FileTypeRegular {
		return 0, ErrNotSupported
	}

	maxLength := fileMeta.Length
	if offset >= maxLength {
		return 0, nil
	}
	if maxLength >= offset+int64(len(data)) {
		maxLength = offset + int64(len(data))
	}

	var bytesWritten int64

	lowerPage := offset >> int64(c.pagePower)
	upperPage := maxLength >> int64(c.pagePower)
	for page := lowerPage; page <= upperPage; page++ {
		pageOffset := offset - (page << c.pagePower)
		if pageOffset < 0 {
			pageOffset = 0
		}

		expectedPageLength := int64(1<<c.pagePower) - pageOffset
		pagePos := (page << int64(c.pagePower))
		if maxLength-pagePos-pageOffset < expectedPageLength {
			expectedPageLength = maxLength - pagePos - pageOffset
		}

		// log.Printf("reading page %d with offset %d local offset %d buffer size %d determined max length %d expected pagelen %d", page, offset, pageOffset, len(data), maxLength, expectedPageLength)

		pageData, err := c.readPage(ctx, fileID, uint32(page))
		if err != nil {
			return int(bytesWritten), errors.Wrapf(err, "readPage in ReadFile for file %q page %d", fileID, page)
		}

		n := int64(copy(data[bytesWritten:], pageData[:expectedPageLength]))
		if n < expectedPageLength {
			copy(data[bytesWritten+n:], make([]byte, expectedPageLength-int64(n)))
		}
		bytesWritten += expectedPageLength
	}

	return int(bytesWritten), nil
}

func (c *SQLBackedClient) writePage(ctx context.Context, fileID uuid.UUID, page uint32, offset uint64, data []byte) error {
	for {
		err := c.PagePool.WritePage(PageKey{
			FileID:  fileID,
			PageNum: page,
		}, offset+uint64(len(data)), func(existing []byte) {
			copy(existing[offset:], data)
			// log.Printf("I just wrote inside %q at page %d offset %d with contents %q", fileID, page, offset, existing)
		})
		if errors.Is(err, ErrTooMuchDirt) {
			log.Println("too much dirt to write page, retrying:", fileID, page)
			time.Sleep(time.Millisecond * 100)
			continue
		} else if errors.Is(err, ErrNotFound) {
			_, err := c.readPage(ctx, fileID, page)
			if err != nil {
				return errors.Wrap(err, "readPage in writePage")
			}
			continue
		} else if err != nil {
			return errors.Wrap(err, "PagePool.WritePage")
		}

		break
	}

	return nil
}

func (c *SQLBackedClient) writeFileNoLock(ctx context.Context, fileMeta *FileMeta, offset int64, data []byte) error {
	if fileMeta.FileType != FileTypeRegular {
		return ErrNotSupported
	}

	maxLength := offset + int64(len(data))

	var bytesWritten int64

	lowerPage := offset >> int64(c.pagePower)
	upperPage := maxLength >> int64(c.pagePower)
	for page := lowerPage; page <= upperPage; page++ {
		pageOffset := offset - (page << c.pagePower)
		if pageOffset < 0 {
			pageOffset = 0
		}

		expectedPageLength := int64(1<<c.pagePower) - pageOffset
		pagePos := (page << int64(c.pagePower))
		if pagePos+pageOffset+expectedPageLength > maxLength {
			expectedPageLength = maxLength - pagePos - pageOffset
		}

		err := c.writePage(ctx, fileMeta.FileID, uint32(page), uint64(pageOffset),
			data[bytesWritten:bytesWritten+expectedPageLength])
		if err != nil {
			return errors.Wrapf(err, "writePage in WriteFile for file %q page %d", fileMeta.FileID, page)
		}

		bytesWritten += expectedPageLength
	}

	c.FileMetaPool.MarkWrite(fileMeta)

	// determine if file needs to be extended
	if maxLength > fileMeta.Length {
		c.FileMetaPool.UpdateLength(fileMeta.FileID, offset+int64(len(data)))
	}

	return nil
}

func (c *SQLBackedClient) WriteFile(ctx context.Context, fileID uuid.UUID, offset int64, data []byte) error {
	if offset < 0 {
		return errors.New("offset must be positive")
	}

	lock := c.getFileLock(fileID)
	lock.Lock()
	defer func() {
		lock.Unlock()
		lock.Release()
	}()

	fileMeta, err := c.GetFile(ctx, fileID)
	if err != nil {
		return err
	}

	return c.writeFileNoLock(ctx, fileMeta, offset, data)
}
