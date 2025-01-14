// Code generated by moq; DO NOT EDIT.
// github.com/matryer/moq

package mocks

import (
	"sync"
)

// SpamWebMock is a mock implementation of events.SpamWeb.
//
//	func TestSomethingThatUsesSpamWeb(t *testing.T) {
//
//		// make and configure a mocked events.SpamWeb
//		mockedSpamWeb := &SpamWebMock{
//			UnbanURLFunc: func(userID int64, msg string) string {
//				panic("mock out the UnbanURL method")
//			},
//		}
//
//		// use mockedSpamWeb in code that requires events.SpamWeb
//		// and then make assertions.
//
//	}
type SpamWebMock struct {
	// UnbanURLFunc mocks the UnbanURL method.
	UnbanURLFunc func(userID int64, msg string) string

	// calls tracks calls to the methods.
	calls struct {
		// UnbanURL holds details about calls to the UnbanURL method.
		UnbanURL []struct {
			// UserID is the userID argument value.
			UserID int64
			// Msg is the msg argument value.
			Msg string
		}
	}
	lockUnbanURL sync.RWMutex
}

// UnbanURL calls UnbanURLFunc.
func (mock *SpamWebMock) UnbanURL(userID int64, msg string) string {
	if mock.UnbanURLFunc == nil {
		panic("SpamWebMock.UnbanURLFunc: method is nil but SpamWeb.UnbanURL was just called")
	}
	callInfo := struct {
		UserID int64
		Msg    string
	}{
		UserID: userID,
		Msg:    msg,
	}
	mock.lockUnbanURL.Lock()
	mock.calls.UnbanURL = append(mock.calls.UnbanURL, callInfo)
	mock.lockUnbanURL.Unlock()
	return mock.UnbanURLFunc(userID, msg)
}

// UnbanURLCalls gets all the calls that were made to UnbanURL.
// Check the length with:
//
//	len(mockedSpamWeb.UnbanURLCalls())
func (mock *SpamWebMock) UnbanURLCalls() []struct {
	UserID int64
	Msg    string
} {
	var calls []struct {
		UserID int64
		Msg    string
	}
	mock.lockUnbanURL.RLock()
	calls = mock.calls.UnbanURL
	mock.lockUnbanURL.RUnlock()
	return calls
}

// ResetUnbanURLCalls reset all the calls that were made to UnbanURL.
func (mock *SpamWebMock) ResetUnbanURLCalls() {
	mock.lockUnbanURL.Lock()
	mock.calls.UnbanURL = nil
	mock.lockUnbanURL.Unlock()
}

// ResetCalls reset all the calls that were made to all mocked methods.
func (mock *SpamWebMock) ResetCalls() {
	mock.lockUnbanURL.Lock()
	mock.calls.UnbanURL = nil
	mock.lockUnbanURL.Unlock()
}
