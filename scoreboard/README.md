# Gardener Scoreboard

The scoreboard keeps track of the state of all parsing activities, persists
the data in datastore, and recovers the scoreboard state from datastore on
startup or recovery.

The scoreboard is used by other components of Gardener to decide:

1. what jobs to do next,
1. when a job has failed and needs to be recovered,
1. when postprocessing actions should be initiated.

The scoreboard provides an API to the other Gardener components to answer
questions about the system state.

1.
1.
1.