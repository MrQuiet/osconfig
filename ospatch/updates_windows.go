//  Copyright 2018 Google Inc. All Rights Reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

//+build !test

package ospatch

import (
	"fmt"

	"github.com/GoogleCloudPlatform/guest-logging-go/logger"
	"github.com/GoogleCloudPlatform/osconfig/inventory/packages"
	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows/registry"
)

// SystemRebootRequired checks whether a system reboot is required.
func SystemRebootRequired() (bool, error) {
	// https://docs.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-movefileexw#remarks
	logger.Debugf("Checking for PendingFileRenameOperations")
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager`, registry.QUERY_VALUE)
	if err == nil {
		val, _, err := k.GetStringsValue("PendingFileRenameOperations")
		if err == nil {
			k.Close()

			if len(val) > 0 {
				logger.Debugf("PendingFileRenameOperations indicate a reboot is required: %q", val)
				return true, nil
			}
		} else if err != registry.ErrNotExist {
			return false, err
		}
	} else if err != registry.ErrNotExist {
		return false, err
	}

	regKeys := []string{
		`SOFTWARE\Microsoft\Windows\CurrentVersion\WindowsUpdate\Auto Update\RebootRequired`,
		`SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending`,
	}
	for _, key := range regKeys {
		logger.Debugf("Checking if reboot required by testing the existance of %s", key)
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, key, registry.QUERY_VALUE)
		if err == nil {
			k.Close()
			logger.Debugf("%s exists indicating a reboot is required.", key)
			return true, nil
		} else if err != registry.ErrNotExist {
			return false, err
		}
	}

	return false, nil
}

func getIterativeProp(src *packages.IUpdate, prop string) (*ole.IDispatch, int32, error) {
	raw, err := src.GetProperty(prop)
	if err != nil {
		return nil, 0, err
	}
	dis := raw.ToIDispatch()

	countRaw, err := dis.GetProperty("Count")
	if err != nil {
		return nil, 0, err
	}
	count, _ := countRaw.Value().(int32)

	return dis, count, nil
}

func checkFilters(updt *packages.IUpdate, kbExcludes, classFilter, exclusive_patches []string) (bool, error) {
	title, err := updt.GetProperty("Title")
	if err != nil {
		return false, fmt.Errorf(`updt.GetProperty("Title"): %v`, err)
	}

	kbArticleIDs, kbArticleIDsCount, err := getIterativeProp(updt, "KBArticleIDs")
	if err != nil {
		return false, fmt.Errorf(`getIterativeProp(updt, "KBArticleIDs"): %v`, err)
	}

	if len(exclusive_patches) > 0 {
		for i := 0; i < int(kbArticleIDsCount); i++ {
			kbRaw, err := kbArticleIDs.GetProperty("Item", i)
			if err != nil {
				return false, err
			}
			for _, e := range exclusive_patches {
				if e == kbRaw.ToString() {
					// until now we have only seen at most 1 kbarticles
					// in a patch update. So, if we get a match, we just
					// install the update
					return true, nil
				}
			}
		}
		// since there are exclusive_patches to be installed,
		// other fields like excludes, classfilter are void
		return false, nil
	}

	if len(kbExcludes) > 0 {
		for i := 0; i < int(kbArticleIDsCount); i++ {
			kbRaw, err := kbArticleIDs.GetProperty("Item", i)
			if err != nil {
				return false, err
			}
			for _, e := range kbExcludes {
				if e == kbRaw.ToString() {
					logger.Debugf("Update %s (%s) matched exclude filter", title.ToString(), kbRaw.ToString())
					return false, nil
				}
			}
		}
	}

	if len(classFilter) == 0 {
		return true, nil
	}

	categories, categoriesCount, err := getIterativeProp(updt, "Categories")
	if err != nil {
		return false, fmt.Errorf(`getIterativeProp(updt, "Categories"): %v`, err)
	}

	for i := 0; i < int(categoriesCount); i++ {
		catRaw, err := categories.GetProperty("Item", i)
		if err != nil {
			return false, err
		}

		catIdRaw, err := catRaw.ToIDispatch().GetProperty("CategoryID")
		if err != nil {
			return false, fmt.Errorf(`catRaw.ToIDispatch().GetProperty("CategoryID"): %v`, err)
		}

		for _, c := range classFilter {
			if c == catIdRaw.ToString() {
				return true, nil
			}
		}
	}

	logger.Debugf("Update %s not found in classification filter", title.ToString())
	return false, nil
}

// GetWUAUpdates gets WUA updates based on optional classFilter and kbExcludes.
func GetWUAUpdates(session *packages.IUpdateSession, classFilter, kbExcludes, exclusive_patches []string) (*packages.IUpdateCollection, error) {
	// Search for all not installed updates but filter out ones that will be installed after a reboot.
	updts, err := session.GetWUAUpdateCollection("IsInstalled=0 AND RebootRequired=0")
	if err != nil {
		return nil, fmt.Errorf("GetWUAUpdateCollection error: %v", err)
	}

	count, err := updts.Count()
	if err != nil {
		return nil, err
	}

	if len(classFilter) == 0 && len(kbExcludes) == 0 {
		return updts, nil
	}

	defer updts.Release()

	updateCollObj, err := oleutil.CreateObject("Microsoft.Update.UpdateColl")
	if err != nil {
		return nil, fmt.Errorf(`oleutil.CreateObject("updateColl"): %v`, err)
	}

	updateColl, err := updateCollObj.IDispatch(ole.IID_IDispatch)
	if err != nil {
		return nil, err
	}

	newUpdts := &packages.IUpdateCollection{IDispatch: updateColl}

	for i := 0; i < int(count); i++ {
		updt, err := updts.Item(i)
		if err != nil {
			return nil, err
		}

		ok, err := checkFilters(updt, kbExcludes, classFilter, exclusive_patches)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		if err := newUpdts.Add(updt); err != nil {
			return nil, err
		}
	}

	return newUpdts, nil
}
