// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2014-2015 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package devicestate

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/asserts/snapasserts"
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/i18n"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/overlord/assertstate"
	"github.com/snapcore/snapd/overlord/auth"
	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/release"
	"github.com/snapcore/snapd/snap"
)

var errNothingToDo = errors.New("nothing to do")

func installSeedSnap(st *state.State, sn *snap.SeedSnap, flags snapstate.Flags) (*state.TaskSet, error) {
	if sn.Classic {
		flags.Classic = true
	}
	if sn.DevMode {
		flags.DevMode = true
	}

	path := filepath.Join(dirs.SnapSeedDir, "snaps", sn.File)

	var sideInfo snap.SideInfo
	if sn.Unasserted {
		sideInfo.RealName = sn.Name
	} else {
		si, err := snapasserts.DeriveSideInfo(path, assertstate.DB(st))
		if asserts.IsNotFound(err) {
			return nil, fmt.Errorf("cannot find signatures with metadata for snap %q (%q)", sn.Name, path)
		}
		if err != nil {
			return nil, err
		}
		sideInfo = *si
		sideInfo.Private = sn.Private
		sideInfo.Contact = sn.Contact
	}

	return snapstate.InstallPath(st, &sideInfo, path, sn.Channel, flags)
}

func populateStateFromSeedImpl(st *state.State) ([]*state.TaskSet, error) {
	// check that the state is empty
	var seeded bool
	err := st.Get("seeded", &seeded)
	if err != nil && err != state.ErrNoState {
		return nil, err
	}
	if seeded {
		return nil, fmt.Errorf("cannot populate state: already seeded")
	}

	markSeeded := st.NewTask("mark-seeded", i18n.G("Mark system seeded"))

	// ack all initial assertions
	model, err := importAssertionsFromSeed(st)
	if err == errNothingToDo {
		return []*state.TaskSet{state.NewTaskSet(markSeeded)}, nil
	}
	if err != nil {
		return nil, err
	}

	seedYamlFile := filepath.Join(dirs.SnapSeedDir, "seed.yaml")
	if release.OnClassic && !osutil.FileExists(seedYamlFile) {
		// on classic it is ok to not seed any snaps
		return []*state.TaskSet{state.NewTaskSet(markSeeded)}, nil
	}

	seed, err := snap.ReadSeedYaml(seedYamlFile)
	if err != nil {
		return nil, err
	}

	var required map[string]bool
	reqSnaps := model.RequiredSnaps()
	if len(reqSnaps) > 0 {
		required = make(map[string]bool, len(reqSnaps))
		for _, snap := range reqSnaps {
			required[snap] = true
		}
	}

	seeding := make(map[string]*snap.SeedSnap, len(seed.Snaps))
	for _, sn := range seed.Snaps {
		seeding[sn.Name] = sn
	}
	alreadySeeded := make(map[string]bool, 3)

	tsAll := []*state.TaskSet{}
	configTss := []*state.TaskSet{}

	// if there are snaps to seed, core needs to be seeded too
	if len(seed.Snaps) != 0 {
		coreSeed := seeding["core"]
		if coreSeed == nil {
			return nil, fmt.Errorf("cannot proceed without seeding core")
		}
		ts, err := installSeedSnap(st, coreSeed, snapstate.Flags{SkipConfigure: true})
		if err != nil {
			return nil, err
		}
		tsAll = append(tsAll, ts)
		alreadySeeded["core"] = true
		configTss = append(configTss, snapstate.ConfigureSnap(st, "core", snapstate.UseConfigDefaults))
	}

	last := 0
	if kernelName := model.Kernel(); kernelName != "" {
		kernelSeed := seeding[kernelName]
		if kernelSeed == nil {
			return nil, fmt.Errorf("cannot find seed information for kernel snap %q", kernelName)
		}
		ts, err := installSeedSnap(st, kernelSeed, snapstate.Flags{SkipConfigure: true})
		if err != nil {
			return nil, err
		}
		ts.WaitAll(tsAll[last])
		tsAll = append(tsAll, ts)
		alreadySeeded[kernelName] = true
		configTs := snapstate.ConfigureSnap(st, kernelName, snapstate.UseConfigDefaults)
		configTs.WaitAll(configTss[last])
		configTss = append(configTss, configTs)
		last++
	}

	if gadgetName := model.Gadget(); gadgetName != "" {
		gadgetSeed := seeding[gadgetName]
		if gadgetSeed == nil {
			return nil, fmt.Errorf("cannot find seed information for gadget snap %q", gadgetName)
		}
		ts, err := installSeedSnap(st, gadgetSeed, snapstate.Flags{SkipConfigure: true})
		if err != nil {
			return nil, err
		}
		ts.WaitAll(tsAll[last])
		tsAll = append(tsAll, ts)
		alreadySeeded[gadgetName] = true
		configTs := snapstate.ConfigureSnap(st, gadgetName, snapstate.UseConfigDefaults)
		configTs.WaitAll(configTss[last])
		configTss = append(configTss, configTs)
		last++
	}

	// chain together configuring core, kernel, and gadget after
	// installing them so that defaults are availabble from gadget
	configTss[0].WaitAll(tsAll[last])
	tsAll = append(tsAll, configTss...)
	last += len(configTss)

	for _, sn := range seed.Snaps {
		if alreadySeeded[sn.Name] {
			continue
		}

		var flags snapstate.Flags
		if required[sn.Name] {
			flags.Required = true
		}

		ts, err := installSeedSnap(st, sn, flags)
		if err != nil {
			return nil, err
		}

		ts.WaitAll(tsAll[last])
		tsAll = append(tsAll, ts)
		last++
	}

	if len(tsAll) == 0 {
		return nil, fmt.Errorf("cannot proceed, no snaps to seed")
	}

	ts := tsAll[len(tsAll)-1]
	markSeeded.WaitAll(ts)
	tsAll = append(tsAll, state.NewTaskSet(markSeeded))

	return tsAll, nil
}

func readAsserts(fn string, batch *assertstate.Batch) ([]*asserts.Ref, error) {
	f, err := os.Open(fn)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return batch.AddStream(f)
}

func importAssertionsFromSeed(st *state.State) (*asserts.Model, error) {
	device, err := auth.Device(st)
	if err != nil {
		return nil, err
	}

	// set device,model from the model assertion
	assertSeedDir := filepath.Join(dirs.SnapSeedDir, "assertions")
	dc, err := ioutil.ReadDir(assertSeedDir)
	if release.OnClassic && os.IsNotExist(err) {
		// on classic seeding is optional
		return nil, errNothingToDo
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read assert seed dir: %s", err)
	}

	// collect
	var modelRef *asserts.Ref
	batch := assertstate.NewBatch()
	for _, fi := range dc {
		fn := filepath.Join(assertSeedDir, fi.Name())
		refs, err := readAsserts(fn, batch)
		if err != nil {
			return nil, fmt.Errorf("cannot read assertions: %s", err)
		}
		for _, ref := range refs {
			if ref.Type == asserts.ModelType {
				if modelRef != nil && modelRef.Unique() != ref.Unique() {
					return nil, fmt.Errorf("cannot add more than one model assertion")
				}
				modelRef = ref
			}
		}
	}
	// verify we have one model assertion
	if modelRef == nil {
		return nil, fmt.Errorf("need a model assertion")
	}

	if err := batch.Commit(st); err != nil {
		return nil, err
	}

	a, err := modelRef.Resolve(assertstate.DB(st).Find)
	if err != nil {
		return nil, fmt.Errorf("internal error: cannot find just added assertion %v: %v", modelRef, err)
	}
	modelAssertion := a.(*asserts.Model)

	classicModel := modelAssertion.Classic()
	if release.OnClassic != classicModel {
		var msg string
		if classicModel {
			msg = "cannot seed an all-snaps system with a classic model"
		} else {
			msg = "cannot seed a classic system with an all-snaps model"
		}
		return nil, fmt.Errorf(msg)
	}

	// set device,model from the model assertion
	device.Brand = modelAssertion.BrandID()
	device.Model = modelAssertion.Model()
	if err := auth.SetDevice(st, device); err != nil {
		return nil, err
	}

	return modelAssertion, nil
}
